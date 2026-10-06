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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A hub-managed project's .scion marker only has to agree with the location
// computed from the project directory name and the requested project ID;
// these tests use markers that disagree in different ways and check that the
// project is never resolved to (or acted on at) any other location.

// writeHubMarker writes marker as ~/.scion/projects/<dirName>/.scion.
func writeHubMarker(t *testing.T, home, dirName string, marker config.ProjectMarker) {
	t.Helper()
	root := filepath.Join(home, ".scion", "projects", dirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), &marker); err != nil {
		t.Fatal(err)
	}
}

// mkAgentDir creates <scionDir>/agents/<name>/home and returns the agent dir.
func mkAgentDir(t *testing.T, scionDir, name string) string {
	t.Helper()
	dir := filepath.Join(scionDir, "agents", name)
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// escapeTo returns a relative slug that, joined under
// <home>/.scion/project-configs, points at the absolute path target.
func escapeTo(target string) string {
	return strings.Repeat("../", 64) + strings.TrimPrefix(target, "/")
}

func shortA() string { return config.ProjectMarker{ProjectID: scopeProjA}.ShortUUID() }

func TestFindAgentInHubManagedProjects_CraftedMarkers(t *testing.T) {
	cases := []struct {
		name string
		// setup writes the crafted project and returns the external dir
		// (holding agents/dev) that must not be resolved.
		setup func(t *testing.T, home string) string
	}{
		{
			name: "slug with parent pointers leaving project-configs",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "../../elsewhere"})
				return filepath.Join(home, "elsewhere__"+shortA(), ".scion")
			},
		},
		{
			name: "slug pointing at another project-configs entry",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "x/../victim"})
				return filepath.Join(home, ".scion", "project-configs", "victim__"+shortA(), ".scion")
			},
		},
		{
			name: "slug escaping to an absolute path",
			setup: func(t *testing.T, home string) string {
				outside := t.TempDir()
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: escapeTo(filepath.Join(outside, "x"))})
				return filepath.Join(outside, "x__"+shortA(), ".scion")
			},
		},
		{
			name: "symlinked project-configs entry",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a"})
				outside := t.TempDir()
				configs := filepath.Join(home, ".scion", "project-configs")
				if err := os.MkdirAll(configs, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(configs, "proj-a__"+shortA())); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(outside, ".scion")
			},
		},
		{
			name: "project-configs entry symlinked to a sibling entry with the same short ID",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a"})
				configs := filepath.Join(home, ".scion", "project-configs")
				sibling := filepath.Join(configs, "other__"+shortA())
				if err := os.MkdirAll(sibling, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sibling, filepath.Join(configs, "proj-a__"+shortA())); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(sibling, ".scion")
			},
		},
		{
			name: "trailing-dot slug",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a.", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a."})
				return filepath.Join(home, ".scion", "project-configs", "proj-a.__"+shortA(), ".scion")
			},
		},
		{
			name: "backslash slug",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, `proj\a`, config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: `proj\a`})
				return filepath.Join(home, ".scion", "project-configs", `proj\a__`+shortA(), ".scion")
			},
		},
		{
			name: "marker slug differs from the directory name",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-b"})
				return filepath.Join(home, ".scion", "project-configs", "proj-b__"+shortA(), ".scion")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			target := tc.setup(t, home)
			agentDir := mkAgentDir(t, target, "dev")

			// The resolver itself refuses every crafted project, not just
			// the later agent-dir check in findAgentInHubManagedProjects.
			globalDir := filepath.Join(home, ".scion")
			projects, err := os.ReadDir(filepath.Join(globalDir, "projects"))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range projects {
				markerPath := filepath.Join(globalDir, "projects", p.Name(), config.DotScion)
				if got := hubMarkerProjectScionDir(globalDir, p.Name(), scopeProjA, markerPath); got != "" {
					t.Errorf("hubMarkerProjectScionDir(%q) = %q; want empty", p.Name(), got)
				}
			}
			if got, err := findAgentInHubManagedProjects("dev", scopeProjA); err != nil || got != "" {
				t.Fatalf("crafted marker resolved to %q (err %v); want nothing", got, err)
			}
			rec := doDelete(t, srv, "dev", "projectId="+scopeProjA+"&deleteFiles=true")
			if rec.Code != http.StatusNotFound {
				t.Errorf("delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
			}
			if p := mgr.LastDeleteProjectPath(); p != "" {
				t.Errorf("delete acted on project path %q", p)
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Errorf("%s was removed: %v", agentDir, err)
			}
		})
	}
}

// A symlinked agents/<name> entry inside an otherwise valid external dir is
// not followed out of project-configs.
func TestFindAgentInHubManagedProjects_SymlinkedAgentDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ext, agentDir := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ext, "agents", "other")); err != nil {
		t.Fatal(err)
	}
	if got, err := findAgentInHubManagedProjects("other", scopeProjA); err != nil || got != "" {
		t.Errorf("symlinked agent dir resolved to %q (err %v); want nothing", got, err)
	}
	if got, _ := findAgentInHubManagedProjects("dev", scopeProjA); got != ext {
		t.Errorf("real agent dir %s: got %q, want %q", agentDir, got, ext)
	}
}

// The marker branch consults the structural identity check
// (markerProjectPathTrusted) before accepting a resolved dir.
func TestFindAgentInHubManagedProjects_MarkerBranchUsesTrustedPathCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ext, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")

	orig := markerProjectPathTrusted
	t.Cleanup(func() { markerProjectPathTrusted = orig })
	var calls []string
	markerProjectPathTrusted = func(path, projectID string) bool {
		calls = append(calls, path)
		return false
	}
	if got, err := findAgentInHubManagedProjects("dev", scopeProjA); err != nil || got != "" {
		t.Errorf("with the check refusing: got %q (err %v); want nothing", got, err)
	}
	if len(calls) != 1 || calls[0] != ext {
		t.Errorf("trusted-path check calls = %v, want [%s]", calls, ext)
	}

	markerProjectPathTrusted = orig
	if got, err := findAgentInHubManagedProjects("dev", scopeProjA); err != nil || got != ext {
		t.Errorf("with the real check: got %q (err %v); want %q", got, err, ext)
	}
}

// An external config dir recording another project's ID is not accepted.
func TestFindAgentInHubManagedProjects_ExternalDirRecordsOtherProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ext, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	if err := config.WriteProjectID(ext, scopeProjB); err != nil {
		t.Fatal(err)
	}
	if got, err := findAgentInHubManagedProjects("dev", scopeProjA); err != nil || got != "" {
		t.Errorf("got %q (err %v); want nothing", got, err)
	}
}

func TestExpectedProjectConfigDir(t *testing.T) {
	g := "/h/.scion"
	want := filepath.Join(g, config.ProjectConfigsDir, "proj-a__"+shortA())
	if got := expectedProjectConfigDir(g, "proj-a", scopeProjA); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, slug := range []string{"", "-a", "a-", "A", "a.b", "a.", "..", "a/b", `a\b`, "a_b", "a b", strings.Repeat("a", 64)} {
		if got := expectedProjectConfigDir(g, slug, scopeProjA); got != "" {
			t.Errorf("slug %q: got %q, want empty", slug, got)
		}
	}
	for _, id := range []string{"", "../x", "a/b", "a.b"} {
		if got := expectedProjectConfigDir(g, "proj-a", id); got != "" {
			t.Errorf("project id %q: got %q, want empty", id, got)
		}
	}
}
