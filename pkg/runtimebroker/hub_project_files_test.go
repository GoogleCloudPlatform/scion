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
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for recordHubProjectFiles: which .scion markers and project-configs
// entries a dispatch creates.

const hubFilesProjectID = "a46b3dcd-0000-4000-8000-000000000001"

// newHubFilesTestServer returns a test server and its HOME.
func newHubFilesTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := newTestServerForStartContext(t, testServerConfig())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return srv, home
}

// projectConfigEntries lists the entries of ~/.scion/project-configs.
func projectConfigEntries(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// newHubFilesCheckout creates <tmp>/repo/.scion as a directory (a linked
// project's own config) and returns the repo root and its .scion dir.
func newHubFilesCheckout(t *testing.T) (repo, dotScion string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	dotScion = filepath.Join(repo, config.DotScion)
	if err := os.MkdirAll(dotScion, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, dotScion
}

// A linked project's dispatch carries its provider path, the project's own
// .scion directory, and no slug. Before the fix this wrote a marker file
// <repo>/.scion/.scion with an empty slug and created
// project-configs/__<id8>/.scion/agents. A linked checkout gets neither,
// with or without a slug.
func TestRecordHubProjectFiles_LinkedCheckoutWritesNothing(t *testing.T) {
	for _, slug := range []string{"", "repo"} {
		t.Run("slug "+slug, func(t *testing.T) {
			srv, home := newHubFilesTestServer(t)
			_, dotScion := newHubFilesCheckout(t)

			srv.recordHubProjectFiles(context.Background(), startContextInputs{
				Name:        "a1",
				ProjectPath: dotScion,
				ProjectSlug: slug,
				ProjectID:   hubFilesProjectID,
				Operation:   opCreate,
			})

			if _, err := os.Lstat(filepath.Join(dotScion, config.DotScion)); !os.IsNotExist(err) {
				t.Errorf("stray marker written inside the linked checkout's .scion (lstat err %v)", err)
			}
			if got := projectConfigEntries(t, home); len(got) != 0 {
				t.Errorf("linked checkout created project-configs entries %v", got)
			}
			if _, err := os.Stat(filepath.Join(dotScion, "project-id")); !os.IsNotExist(err) {
				t.Errorf("project-id written into the linked checkout's .scion (stat err %v)", err)
			}
		})
	}
}

// The same dispatch through buildStartContext, the path agent start takes.
func TestBuildStartContext_LinkedCheckoutWritesNoMarkerOrExternalDir(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	_, dotScion := newHubFilesCheckout(t)

	_, _ = srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "a1",
		ProjectPath: dotScion,
		ProjectID:   hubFilesProjectID,
		Operation:   opCreate,
	})

	if _, err := os.Lstat(filepath.Join(dotScion, config.DotScion)); !os.IsNotExist(err) {
		t.Errorf("stray marker written inside the linked checkout's .scion (lstat err %v)", err)
	}
	if got := projectConfigEntries(t, home); len(got) != 0 {
		t.Errorf("linked checkout created project-configs entries %v", got)
	}
}

// A project path without .scion and a dispatch without a slug: no marker,
// no project dir and no "__<id8>" external dir.
func TestRecordHubProjectFiles_EmptySlugWritesNothing(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	projectPath := filepath.Join(t.TempDir(), "plain")

	srv.recordHubProjectFiles(context.Background(), startContextInputs{
		Name:        "a1",
		ProjectPath: projectPath,
		ProjectID:   hubFilesProjectID,
		Operation:   opCreate,
	})

	if _, err := os.Lstat(projectPath); !os.IsNotExist(err) {
		t.Errorf("project dir or marker created for an empty slug (lstat err %v)", err)
	}
	if got := projectConfigEntries(t, home); len(got) != 0 {
		t.Errorf("empty slug created project-configs entries %v", got)
	}
}

// A hub-managed project (~/.scion/projects/<slug>) still gets its marker and
// <slug>__<id8>/.scion/agents.
func TestRecordHubProjectFiles_HubManagedUnchanged(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	projectPath := filepath.Join(home, config.GlobalDir, "projects", "proj")

	srv.recordHubProjectFiles(context.Background(), startContextInputs{
		Name:        "a1",
		ProjectPath: projectPath,
		ProjectSlug: "proj",
		ProjectID:   hubFilesProjectID,
		Operation:   opCreate,
	})

	marker, err := config.ReadProjectMarker(filepath.Join(projectPath, config.DotScion))
	if err != nil {
		t.Fatalf("hub-managed marker not readable: %v", err)
	}
	if marker.ProjectID != hubFilesProjectID || marker.ProjectSlug != "proj" {
		t.Errorf("unexpected marker %+v", marker)
	}
	want := "proj__" + marker.ShortUUID()
	if info, err := os.Stat(filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, want, config.DotScion, "agents")); err != nil || !info.IsDir() {
		t.Errorf("expected %s/.scion/agents (stat err %v)", want, err)
	}
	if got := projectConfigEntries(t, home); len(got) != 1 || got[0] != want {
		t.Errorf("project-configs entries = %v, want [%s]", got, want)
	}
}

// A git project whose root holds a .scion directory still records the hub's
// project ID and creates its external dirs.
func TestRecordHubProjectFiles_GitProjectRootUnchanged(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	repo, dotScion := newHubFilesCheckout(t)

	srv.recordHubProjectFiles(context.Background(), startContextInputs{
		Name:        "a1",
		ProjectPath: repo,
		ProjectID:   hubFilesProjectID,
		Operation:   opCreate,
	})

	if id, err := config.ReadProjectID(dotScion); err != nil || id != hubFilesProjectID {
		t.Errorf("project-id = %q, %v; want %q", id, err, hubFilesProjectID)
	}
	if got := projectConfigEntries(t, home); len(got) != 1 {
		t.Errorf("project-configs entries = %v, want one", got)
	}
}

// The global directory keeps its marker for the global project, which may
// carry an empty slug, but gets no "__<id8>" external dir.
func TestRecordHubProjectFiles_GlobalEmptySlugMarkerNoExternalDir(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	globalDir := filepath.Join(home, config.GlobalDir)
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	srv.recordHubProjectFiles(context.Background(), startContextInputs{
		Name:             "a1",
		ProjectPath:      globalDir,
		ProjectID:        hubFilesProjectID,
		HubGlobalProject: true,
		Operation:        opCreate,
	})

	if !config.IsProjectMarkerFile(filepath.Join(globalDir, config.DotScion)) {
		t.Error("expected the global marker to be written")
	}
	if got := projectConfigEntries(t, home); len(got) != 0 {
		t.Errorf("empty-slug global marker created project-configs entries %v", got)
	}
}

// A provider path <repo>/.scion that no longer exists, with no slug: the
// .scion directory is not recreated and no marker or external dir is written.
func TestRecordHubProjectFiles_MissingProviderScionDirWritesNothing(t *testing.T) {
	srv, home := newHubFilesTestServer(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	dotScion := filepath.Join(repo, config.DotScion)

	srv.recordHubProjectFiles(context.Background(), startContextInputs{
		Name:        "a1",
		ProjectPath: dotScion,
		ProjectID:   hubFilesProjectID,
		Operation:   opCreate,
	})

	if _, err := os.Lstat(dotScion); !os.IsNotExist(err) {
		t.Errorf("missing provider .scion was recreated (lstat err %v)", err)
	}
	if got := projectConfigEntries(t, home); len(got) != 0 {
		t.Errorf("missing provider .scion created project-configs entries %v", got)
	}
}
