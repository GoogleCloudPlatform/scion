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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const (
	relinkOldID   = "11111111-aaaa-4aaa-8aaa-111111111111"
	relinkNewID   = "22222222-bbbb-4bbb-8bbb-222222222222"
	relinkOtherID = "33333333-cccc-4ccc-8ccc-333333333333"
	relinkAgent   = "relink-agent"
	relinkState   = "kept agent state\n"
)

// relinkFixture is a shared-workspace project "proj" set up with a .scion
// marker file naming relinkOldID. The project config directory for that ID
// holds the agent's state and a settings.yaml whose workspace_path names the
// workspace.
type relinkFixture struct {
	srv              *Server
	mgr              *mockManager
	root, dotScion   string
	oldRoot, newRoot string
}

func newRelinkFixture(t *testing.T) *relinkFixture {
	t.Helper()
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "proj")
	dotScion := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	writeRelinkMarker(t, dotScion, relinkOldID, "proj")
	oldRoot, ok := config.ConfinedProjectConfigRoot("proj", relinkOldID)
	newRoot, ok2 := config.ConfinedProjectConfigRoot("proj", relinkNewID)
	if !ok || !ok2 {
		t.Fatal("project config roots")
	}
	writeRelinkSettings(t, filepath.Join(oldRoot, config.DotScion), root)
	agentDir := filepath.Join(oldRoot, config.DotScion, "agents", relinkAgent)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "state.txt"), relinkState)
	writeRelinkFile(t, filepath.Join(agentDir, "scion-agent.json"), `{}`)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "agent-info.json"), `{"name": "`+relinkAgent+`"}`)
	writeRelinkFile(t, filepath.Join(oldRoot, config.SharedDirsSubdir, "cache", "f"), "shared\n")
	if err := os.Symlink("state.txt", filepath.Join(agentDir, "home", "link")); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	mgr := &mockManager{agents: []api.AgentInfo{{
		ID: relinkAgent, Name: relinkAgent, ContainerID: relinkAgent, ProjectPath: filepath.Join(oldRoot, config.DotScion), Phase: "stopped",
		Labels: map[string]string{"scion.name": relinkAgent, "scion.project_id": relinkOldID},
	}}}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	return &relinkFixture{srv: srv, mgr: mgr, root: root, dotScion: dotScion, oldRoot: oldRoot, newRoot: newRoot}
}

// writeRelinkSettings writes a project settings.yaml into configDir,
// recording workspacePath as workspace_path unless it is empty.
func writeRelinkSettings(t *testing.T, configDir, workspacePath string) {
	t.Helper()
	content := `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: local-docker
runtimes:
  local-docker:
    type: docker
`
	if workspacePath != "" {
		content += "workspace_path: " + strconvQuote(workspacePath) + "\n"
	}
	writeProjectSettings(t, configDir, content)
}

// writeRelinkMarker writes a .scion marker file.
func writeRelinkMarker(t *testing.T, path, id, slug string) {
	t.Helper()
	if err := config.WriteProjectMarker(path, &config.ProjectMarker{ProjectID: id, ProjectName: slug, ProjectSlug: slug}); err != nil {
		t.Fatal(err)
	}
}

func writeRelinkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f *relinkFixture) relink(slug string) error {
	return f.srv.relinkSharedProjectIdentity(context.Background(), f.root, slug, relinkNewID, relinkAgent)
}

func (f *relinkFixture) markerID(t *testing.T) string {
	t.Helper()
	m, err := config.ReadProjectMarker(f.dotScion)
	if err != nil {
		t.Fatal(err)
	}
	return m.ProjectID
}

// assertState checks that the agent's seeded state is complete under root.
func assertState(t *testing.T, root string) {
	t.Helper()
	home := filepath.Join(root, config.DotScion, "agents", relinkAgent, "home")
	if got, err := os.ReadFile(filepath.Join(home, "state.txt")); err != nil || string(got) != relinkState {
		t.Fatalf("agent state under %s: %q, %v", root, got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, config.SharedDirsSubdir, "cache", "f")); err != nil || string(got) != "shared\n" {
		t.Fatalf("shared dir under %s: %q, %v", root, got, err)
	}
	if link, err := os.Readlink(filepath.Join(home, "link")); err != nil || link != "state.txt" {
		t.Fatalf("symlink under %s: %q, %v", root, link, err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

// forceCopyPath makes the move take the copy-and-verify path.
func forceCopyPath(t *testing.T) {
	t.Helper()
	prev := relinkRenameDir
	relinkRenameDir = func(_, _ string) error { return errAtomicRenameUnavailable }
	t.Cleanup(func() { relinkRenameDir = prev })
}

// TestRelink_SameProjectMovesConfigDirAndRewritesMarker: a marker naming an
// earlier ID of the same project is rewritten to the current ID and the
// project config directory, with the agent's state, follows it; the identity
// check then passes.
func TestRelink_SameProjectMovesConfigDirAndRewritesMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		copy bool
		slug string
	}{
		{name: "rename", slug: ""},
		{name: "rename with matching dispatch slug", slug: "proj"},
		{name: "verified copy", copy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			if tc.copy {
				forceCopyPath(t)
				prev := relinkVerifyDir
				relinkVerifyDir = func(src, dst string) error {
					// The old directory is still there while the copy is verified.
					assertState(t, src)
					return prev(src, dst)
				}
				t.Cleanup(func() { relinkVerifyDir = prev })
			}
			if err := f.relink(tc.slug); err != nil {
				t.Fatal(err)
			}
			if got := f.markerID(t); got != relinkNewID {
				t.Fatalf("marker = %q, want %q", got, relinkNewID)
			}
			assertState(t, f.newRoot)
			assertAbsent(t, f.oldRoot)
			if err := verifySharedProjectIdentity(f.root, relinkNewID); err != nil {
				t.Fatalf("identity check after re-link: %v", err)
			}
		})
	}
}

// TestRelink_GitProjectWithoutWorkspacePathConflicts: a git project
// (.scion directory with a project-id file) whose earlier project config
// directory records no workspace_path is not moved and still gets the 409.
func TestRelink_GitProjectWithoutWorkspacePathConflicts(t *testing.T) {
	f := newRelinkFixture(t)
	if err := os.Remove(f.dotScion); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(f.dotScion, relinkOldID); err != nil {
		t.Fatal(err)
	}
	writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), "")
	w := f.start(t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
		t.Fatalf("expected 409 identity mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if id, err := config.ReadProjectID(f.dotScion); err != nil || id != relinkOldID {
		t.Fatalf("project-id = %q, %v", id, err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// TestRelink_GitProjectRecordingWorkspacePathNotMoved: a git project whose
// earlier project config directory records this workspace as
// workspace_path (as init, hub link and the CLI now write) is still not
// moved, its project-id file is not rewritten, and the start still gets the
// 409.
func TestRelink_GitProjectRecordingWorkspacePathNotMoved(t *testing.T) {
	f := newRelinkFixture(t)
	if err := os.Remove(f.dotScion); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(f.dotScion, relinkOldID); err != nil {
		t.Fatal(err)
	}
	writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), f.root)
	if err := f.relink(""); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if id, err := config.ReadProjectID(f.dotScion); err != nil || id != relinkOldID {
		t.Fatalf("project-id = %q, %v", id, err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
	w := f.start(t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
		t.Fatalf("expected 409 identity mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if id, err := config.ReadProjectID(f.dotScion); err != nil || id != relinkOldID {
		t.Fatalf("project-id after start = %q, %v", id, err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// TestRelink_WorkspacePathAnchor: the move happens only when the earlier
// project config directory's settings.yaml records this workspace.
func TestRelink_WorkspacePathAnchor(t *testing.T) {
	t.Run("absent workspace_path does not fall back", func(t *testing.T) {
		f := newRelinkFixture(t)
		writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), "")
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
	t.Run("different workspace_path", func(t *testing.T) {
		f := newRelinkFixture(t)
		other := filepath.Join(t.TempDir(), "proj")
		if err := os.MkdirAll(other, 0755); err != nil {
			t.Fatal(err)
		}
		writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), other)
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
	t.Run("workspace_path naming a missing directory", func(t *testing.T) {
		f := newRelinkFixture(t)
		writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), filepath.Join(t.TempDir(), "gone"))
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
	t.Run("workspace recorded through a symlink does not match", func(t *testing.T) {
		f := newRelinkFixture(t)
		link := filepath.Join(t.TempDir(), "proj-link")
		if err := os.Symlink(f.root, link); err != nil {
			t.Fatal(err)
		}
		writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), link)
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
		if w := f.start(t); w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("workspace dispatched through a symlink does not match", func(t *testing.T) {
		f := newRelinkFixture(t)
		link := filepath.Join(t.TempDir(), "proj-link")
		if err := os.Symlink(f.root, link); err != nil {
			t.Fatal(err)
		}
		if err := f.srv.relinkSharedProjectIdentity(context.Background(), link, "", relinkNewID, relinkAgent); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
}

// nestedSymlinkSetup records another project "other" (relinkOtherID) at
// <workspace>/sub, gives its config directory a same-named agent, replaces
// <workspace>/sub with a symlink to the workspace itself, and points the
// workspace marker at that project. It returns the other project's config
// directory.
func nestedSymlinkSetup(t *testing.T, f *relinkFixture) string {
	t.Helper()
	otherRoot, ok := config.ConfinedProjectConfigRoot("other", relinkOtherID)
	if !ok {
		t.Fatal("other root")
	}
	nested := filepath.Join(f.root, "sub")
	writeRelinkSettings(t, filepath.Join(otherRoot, config.DotScion), nested)
	writeRelinkFile(t, filepath.Join(otherRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt"), "other\n")
	if err := os.Symlink(".", nested); err != nil {
		t.Fatal(err)
	}
	writeRelinkMarker(t, f.dotScion, relinkOtherID, "other")
	return otherRoot
}

// TestRelink_NestedWorkspaceSymlinkMovesNothing: a project recorded at a path
// inside this workspace that a symlink resolves to the workspace itself is
// not this workspace: nothing is moved, the marker is unchanged, and the start
// gets the 409.
func TestRelink_NestedWorkspaceSymlinkMovesNothing(t *testing.T) {
	f := newRelinkFixture(t)
	otherRoot := nestedSymlinkSetup(t, f)
	w := f.start(t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
		t.Fatalf("expected 409 identity mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(otherRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != "other\n" {
		t.Fatalf("other project's dir changed: %q, %v", got, err)
	}
	movedOther, _ := config.ConfinedProjectConfigRoot("other", relinkNewID)
	assertAbsent(t, movedOther)
	if got := f.markerID(t); got != relinkOtherID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// TestRelink_ForeignMarkerMovesNothing: a marker file naming another
// project's slug and ID, with no dispatch slug, where that project's config
// directory holds a same-named agent, moves nothing and still gets the 409.
func TestRelink_ForeignMarkerMovesNothing(t *testing.T) {
	f := newRelinkFixture(t)
	foreignRoot, ok := config.ConfinedProjectConfigRoot("other", relinkOtherID)
	if !ok {
		t.Fatal("foreign root")
	}
	foreignWorkspace := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(foreignWorkspace, 0755); err != nil {
		t.Fatal(err)
	}
	writeRelinkSettings(t, filepath.Join(foreignRoot, config.DotScion), foreignWorkspace)
	writeRelinkFile(t, filepath.Join(foreignRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt"), "foreign\n")
	writeRelinkMarker(t, f.dotScion, relinkOtherID, "other")

	w := f.start(t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
		t.Fatalf("expected 409 identity mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(foreignRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != "foreign\n" {
		t.Fatalf("foreign dir changed: %q, %v", got, err)
	}
	newForeign, _ := config.ConfinedProjectConfigRoot("other", relinkNewID)
	assertAbsent(t, newForeign)
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOtherID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// start posts a shared-workspace start for the fixture's agent with the
// current Hub project ID and no project slug.
func (f *relinkFixture) start(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
		strings.NewReader(`{"sharedWorkspace": true, "projectPath": `+strconvQuote(f.root)+`}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

// TestRelink_StartResumesWithMovedState: a shared-workspace start whose
// marker and config directory use an earlier ID of the agent's project is
// not refused; it resumes with the state at the current ID's location.
func TestRelink_StartResumesWithMovedState(t *testing.T) {
	f := newRelinkFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
		strings.NewReader(`{"sharedWorkspace": true, "projectPath": `+strconvQuote(f.root)+`}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusConflict {
		t.Fatalf("start refused: %s", w.Body.String())
	}
	if got := f.markerID(t); got != relinkNewID {
		t.Fatalf("marker = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
}

// TestRelink_UnrelatedProjectStillConflicts: a marker that names a different
// project is left alone and the dispatch still fails with 409.
func TestRelink_UnrelatedProjectStillConflicts(t *testing.T) {
	t.Run("dispatch slug differs", func(t *testing.T) {
		f := newRelinkFixture(t)
		if err := f.relink("another-project"); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
	t.Run("no state of this agent under the marker's ID", func(t *testing.T) {
		f := newRelinkFixture(t)
		writeRelinkMarker(t, f.dotScion, relinkOtherID, "proj")
		for _, op := range []string{"start"} {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/"+op+"?projectId="+relinkNewID,
				strings.NewReader(`{"sharedWorkspace": true, "projectPath": `+strconvQuote(f.root)+`}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
				t.Fatalf("%s: expected 409 identity mismatch, got %d: %s", op, w.Code, w.Body.String())
			}
		}
		if got := f.markerID(t); got != relinkOtherID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
}

// TestRelink_OldDirWithoutAgentStateIsLeftAlone: when the earlier project
// config directory exists but holds no state of this agent, nothing is
// moved and the dispatch still fails with 409.
func TestRelink_OldDirWithoutAgentStateIsLeftAlone(t *testing.T) {
	f := newRelinkFixture(t)
	agentsDir := filepath.Join(f.oldRoot, config.DotScion, "agents")
	if err := os.Rename(filepath.Join(agentsDir, relinkAgent), filepath.Join(agentsDir, "another-agent")); err != nil {
		t.Fatal(err)
	}
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "another-agent", "home", "state.txt")); err != nil {
		t.Fatalf("old dir changed: %v", err)
	}
	assertAbsent(t, f.newRoot)
	if err := verifySharedProjectIdentity(f.root, relinkNewID); err == nil {
		t.Fatal("expected the identity check to still fail")
	}
}

// TestRelink_InvalidMarkerIDMovesNothing: a marker project ID that is not a
// canonical UUID (path separators, "..", other tokens) moves nothing and the
// dispatch still fails the identity check.
func TestRelink_InvalidMarkerIDMovesNothing(t *testing.T) {
	for _, id := range []string{
		"../../outside",
		"a/b",
		"proj..x",
		"11111111..aaaa",
		"not-a-uuid",
		relinkOldID + "x",
		strings.ReplaceAll(relinkOldID, "-", ""),
		strings.ToUpper(relinkOldID),
		"{" + relinkOldID + "}",
	} {
		t.Run(id, func(t *testing.T) {
			f := newRelinkFixture(t)
			raw := "project-id: " + strconvQuote(id) + "\nproject-name: proj\nproject-slug: proj\n"
			if err := os.WriteFile(f.dotScion, []byte(raw), 0644); err != nil {
				t.Fatal(err)
			}
			if err := f.relink(""); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(f.dotScion); string(got) != raw {
				t.Fatalf("marker changed to %q", got)
			}
			assertState(t, f.oldRoot)
			assertAbsent(t, f.newRoot)
			if err := verifySharedProjectIdentity(f.root, relinkNewID); err == nil {
				t.Fatal("expected the identity check to still fail")
			}
		})
	}
	t.Run("non-UUID hub ID", func(t *testing.T) {
		f := newRelinkFixture(t)
		if err := f.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", "hub.project", relinkAgent); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
	})
}

// TestDirectlyUnderProjectConfigs rejects paths that are not a direct child
// of the project-configs directory.
func TestDirectlyUnderProjectConfigs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(globalDir, config.ProjectConfigsDir)
	if !directlyUnderProjectConfigs(filepath.Join(root, "proj__11111111")) {
		t.Fatal("direct child rejected")
	}
	for _, p := range []string{
		root,
		filepath.Join(root, "proj__11111111", "nested"),
		root + "/proj__1111/../../escaped",
		filepath.Join(root, ".."),
		filepath.Join(globalDir, "other", "proj__11111111"),
		root + "-other/proj__11111111",
		filepath.Join(root+"-other", "proj__11111111"),
		filepath.Join(root, "..", config.ProjectConfigsDir+"-other", "proj__11111111"),
		"/tmp/proj__11111111",
	} {
		if directlyUnderProjectConfigs(p) {
			t.Errorf("%s accepted", p)
		}
	}
}

// TestRelink_TargetExistsRefuses: when a project config directory for the
// current ID already exists, the re-link refuses and leaves both directories
// and the marker unchanged, even when that directory is empty.
func TestRelink_TargetExistsRefuses(t *testing.T) {
	for _, withFile := range []bool{true, false} {
		name := "empty target"
		if withFile {
			name = "target with a file"
		}
		t.Run(name, func(t *testing.T) {
			f := newRelinkFixture(t)
			if err := os.MkdirAll(f.newRoot, 0755); err != nil {
				t.Fatal(err)
			}
			if withFile {
				writeRelinkFile(t, filepath.Join(f.newRoot, "existing"), "target data\n")
			}
			err := f.relink("")
			var re *projectRelinkError
			if !errors.As(err, &re) || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("expected a target-exists refusal, got %v", err)
			}
			if strings.Contains(err.Error(), f.newRoot) {
				t.Errorf("message must not contain the host path: %v", err)
			}
			assertState(t, f.oldRoot)
			if _, statErr := os.Stat(f.newRoot); statErr != nil {
				t.Fatalf("target removed: %v", statErr)
			}
			if withFile {
				if got, _ := os.ReadFile(filepath.Join(f.newRoot, "existing")); string(got) != "target data\n" {
					t.Fatalf("target content changed: %q", got)
				}
			}
			if got := f.markerID(t); got != relinkOldID {
				t.Fatalf("marker rewritten to %q", got)
			}
		})
	}

	// The handler answers 409 with the refusal.
	f := newRelinkFixture(t)
	writeRelinkFile(t, filepath.Join(f.newRoot, "existing"), "target data\n")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
		strings.NewReader(`{"sharedWorkspace": true, "projectPath": `+strconvQuote(f.root)+`}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("expected 409 target exists, got %d: %s", w.Code, w.Body.String())
	}
	if f.mgr.startCalls != 0 {
		t.Fatalf("Start ran after a refused re-link")
	}
	assertState(t, f.oldRoot)
}

// TestRelink_CopyFailureKeepsOldDir: a copy that fails midway (an entry that
// cannot be copied) leaves the old directory and the marker unchanged and
// removes the partial copy.
func TestRelink_CopyFailureKeepsOldDir(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	fifo := filepath.Join(f.oldRoot, config.DotScion, "agents", relinkAgent, "home", "zz-fifo")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := f.relink(""); err == nil {
		t.Fatal("expected the re-link to fail")
	}
	assertState(t, f.oldRoot)
	if _, err := os.Lstat(fifo); err != nil {
		t.Fatalf("old entry removed: %v", err)
	}
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// TestRelink_VerificationFailureKeepsOldDir: when the copy does not verify,
// the old directory is kept and the copy removed.
func TestRelink_VerificationFailureKeepsOldDir(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	prev := relinkVerifyDir
	relinkVerifyDir = func(_, _ string) error { return errors.New("size mismatch") }
	t.Cleanup(func() { relinkVerifyDir = prev })
	if err := f.relink(""); err == nil {
		t.Fatal("expected the re-link to fail")
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// TestVerifyDirCopy detects a missing entry, a size change and a changed
// link target.
func TestVerifyDirCopy(t *testing.T) {
	f := newRelinkFixture(t)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}
	if err := copyDirContents(f.oldRoot, dst); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirCopy(f.oldRoot, dst); err != nil {
		t.Fatalf("faithful copy: %v", err)
	}
	home := filepath.Join(dst, config.DotScion, "agents", relinkAgent, "home")
	for name, mutate := range map[string]func() error{
		"size": func() error { return os.WriteFile(filepath.Join(home, "state.txt"), []byte("x"), 0644) },
		"link": func() error {
			_ = os.Remove(filepath.Join(home, "link"))
			return os.Symlink("other", filepath.Join(home, "link"))
		},
		"absent": func() error { return os.Remove(filepath.Join(dst, config.SharedDirsSubdir, "cache", "f")) },
		"mode":   func() error { return os.Chmod(filepath.Join(dst, config.SharedDirsSubdir, "cache", "f"), 0600) },
	} {
		if err := mutate(); err != nil {
			t.Fatal(err)
		}
		if err := verifyDirCopy(f.oldRoot, dst); err == nil {
			t.Fatalf("%s: expected a verification error", name)
		}
		if err := os.RemoveAll(dst); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dst, 0755); err != nil {
			t.Fatal(err)
		}
		if err := copyDirContents(f.oldRoot, dst); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRelink_MarkerWriteFailureUndoesMove: when the marker cannot be
// rewritten, the move is undone and the old directory is back in place.
func TestRelink_MarkerWriteFailureUndoesMove(t *testing.T) {
	f := newRelinkFixture(t)
	failMarkerWrite(t)
	if err := f.relink(""); err == nil {
		t.Fatal("expected the re-link to fail")
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// TestRelink_RunningAgentOfOldIDRefuses: an agent of the earlier project ID
// that may be running blocks the move.
func TestRelink_RunningAgentOfOldIDRefuses(t *testing.T) {
	f := newRelinkFixture(t)
	f.mgr.agents[0].Phase = "running"
	if err := f.relink(""); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected an in-use refusal, got %v", err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}

// TestRelink_CopyPathKeepsModes: the copy path reproduces permission bits,
// including setgid, regardless of the umask.
func TestRelink_CopyPathKeepsModes(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	shared := filepath.Join(f.oldRoot, config.SharedDirsSubdir, "cache")
	private := filepath.Join(f.oldRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")
	if err := os.Chmod(shared, 0775|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0600); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{}
	for _, p := range []string{shared, private} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want[p] = copiedMode(info.Mode())
	}
	if want[shared]&os.ModeSetgid == 0 {
		t.Skip("setgid cannot be set on this filesystem")
	}
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	for p, m := range want {
		rel, _ := filepath.Rel(f.oldRoot, p)
		info, err := os.Stat(filepath.Join(f.newRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := copiedMode(info.Mode()); got != m {
			t.Errorf("%s: mode %v, want %v", rel, got, m)
		}
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
}

// TestRelink_CopyPathMarkerWriteFailureRemovesCopy: on the copy path, a
// marker that cannot be rewritten removes the copy and keeps the old dir.
func TestRelink_CopyPathMarkerWriteFailureRemovesCopy(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	failMarkerWrite(t)
	err := f.relink("")
	if err == nil || !strings.Contains(err.Error(), "both directories were left unchanged") {
		t.Fatalf("expected a refusal leaving both unchanged, got %v", err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// TestRelink_FailedUndoIsReportedAndRecoveredOnNextStart: when the marker
// cannot be rewritten and undoing the move also fails, the error says the
// directory was moved; the next start, once the marker is writable, finishes
// the re-link by rewriting the marker only.
func TestRelink_FailedUndoIsReportedAndRecoveredOnNextStart(t *testing.T) {
	f := newRelinkFixture(t)
	prev := relinkRenameDir
	calls := 0
	relinkRenameDir = func(oldpath, newpath string) error {
		calls++
		if calls > 1 {
			return errors.New("undo refused")
		}
		return prev(oldpath, newpath)
	}
	t.Cleanup(func() { relinkRenameDir = prev })
	restoreWrite := failMarkerWrite(t)
	err := f.relink("")
	var re *projectRelinkError
	if !errors.As(err, &re) || !re.Moved || strings.Contains(err.Error(), "left unchanged") || !strings.Contains(err.Error(), "could not be undone") {
		t.Fatalf("expected a moved-but-not-undone error, got %v", err)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker = %q", got)
	}

	restoreWrite()
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	if got := f.markerID(t); got != relinkNewID {
		t.Fatalf("marker = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
}

// TestRelink_InterruptedMoveRecovery: a move that completed without its
// marker rewrite is finished by rewriting the marker, but only when the
// current directory holds this agent's state and records this workspace.
func TestRelink_InterruptedMoveRecovery(t *testing.T) {
	interrupted := func(t *testing.T) *relinkFixture {
		f := newRelinkFixture(t)
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		return f
	}
	t.Run("finished", func(t *testing.T) {
		f := interrupted(t)
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkNewID {
			t.Fatalf("marker = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
	})
	t.Run("current dir records another workspace", func(t *testing.T) {
		f := interrupted(t)
		writeRelinkSettings(t, filepath.Join(f.newRoot, config.DotScion), t.TempDir())
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
	})
	t.Run("current dir recorded through a symlink inside the workspace", func(t *testing.T) {
		f := interrupted(t)
		nested := filepath.Join(f.root, "sub")
		if err := os.Symlink(".", nested); err != nil {
			t.Fatal(err)
		}
		writeRelinkSettings(t, filepath.Join(f.newRoot, config.DotScion), nested)
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
	})
	t.Run("current dir holds no state of this agent", func(t *testing.T) {
		f := interrupted(t)
		agents := filepath.Join(f.newRoot, config.DotScion, "agents")
		if err := os.Rename(filepath.Join(agents, relinkAgent), filepath.Join(agents, "another-agent")); err != nil {
			t.Fatal(err)
		}
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
	})
}

// TestRelink_SymlinkedAgentsDirMovesNothing: an agent state path with a
// symlinked component below project-configs is not accepted.
func TestRelink_SymlinkedAgentsDirMovesNothing(t *testing.T) {
	f := newRelinkFixture(t)
	agents := filepath.Join(f.oldRoot, config.DotScion, "agents")
	elsewhere := filepath.Join(t.TempDir(), "agents")
	if err := os.Rename(agents, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, agents); err != nil {
		t.Fatal(err)
	}
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
	assertAbsent(t, f.newRoot)
	if _, err := os.Lstat(agents); err != nil {
		t.Fatalf("old dir changed: %v", err)
	}
}

// failMarkerWrite makes the marker rewrite fail until the returned function
// (also run at cleanup) restores it.
func failMarkerWrite(t *testing.T) func() {
	t.Helper()
	prev := relinkWriteMarker
	relinkWriteMarker = func(*config.WorkspaceIdentity, string, string) error { return errors.New("marker write refused") }
	restore := func() { relinkWriteMarker = prev }
	t.Cleanup(restore)
	return restore
}

// failRemoveTree makes tree removal fail.
func failRemoveTree(t *testing.T) {
	t.Helper()
	prev := relinkRemoveTree
	relinkRemoveTree = func(string) error { return errors.New("removal refused") }
	t.Cleanup(func() { relinkRemoveTree = prev })
}

// TestRelink_PartialCopyIsReported: on the copy path, when the copy cannot be
// removed after a failure, the error says a partial copy remains (not that
// both directories are unchanged, and not that the directory was moved), and
// the earlier directory and the marker are intact.
func TestRelink_PartialCopyIsReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"verification failure", func(t *testing.T) {
			prev := relinkVerifyDir
			relinkVerifyDir = func(_, _ string) error { return errors.New("size mismatch") }
			t.Cleanup(func() { relinkVerifyDir = prev })
		}},
		{"marker write failure", func(t *testing.T) { failMarkerWrite(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			forceCopyPath(t)
			tc.setup(t)
			failRemoveTree(t)
			err := f.relink("")
			var re *projectRelinkError
			if !errors.As(err, &re) || !re.PartialCopy || re.Moved || !strings.Contains(err.Error(), "partial copy remains") || strings.Contains(err.Error(), "left unchanged") {
				t.Fatalf("expected a partial-copy error, got %v", err)
			}
			assertState(t, f.oldRoot)
			if got := f.markerID(t); got != relinkOldID {
				t.Fatalf("marker rewritten to %q", got)
			}
		})
	}
}

// TestRelink_CopyPathRemovesReadOnlyOldDir: after a verified copy, the
// earlier directory is removed even when it holds a read-only directory.
func TestRelink_CopyPathRemovesReadOnlyOldDir(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	ro := filepath.Join(f.oldRoot, config.SharedDirsSubdir, "cache")
	if err := os.Chmod(ro, 0555); err != nil {
		t.Fatal(err)
	}
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
	t.Cleanup(func() { _ = removeTreeWritable(f.newRoot) })
}

// TestRelink_GitProjectSymlinkedIDNotTouched: a git project is never
// re-linked, so a symlinked project-id file is neither written through nor
// replaced, and nothing moves. (The writer's no-follow handling of a
// symlinked project-id file is tested directly in
// TestWriteWorkspaceIdentityNoFollow.)
func TestRelink_GitProjectSymlinkedIDNotTouched(t *testing.T) {
	f := newRelinkFixture(t)
	if err := os.Remove(f.dotScion); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-id")
	if err := os.WriteFile(outside, []byte(relinkOldID+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(f.dotScion, "project-id")
	if err := os.Symlink(outside, idFile); err != nil {
		t.Fatal(err)
	}
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != relinkOldID+"\n" {
		t.Fatalf("symlink target written: %q", got)
	}
	info, err := os.Lstat(idFile)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("project-id symlink replaced: %v, %v", info, err)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// TestWriteWorkspaceIdentityNoFollow refuses a marker whose directory is a
// symlink and replaces a symlinked marker file without writing its target.
func TestWriteWorkspaceIdentityNoFollow(t *testing.T) {
	t.Run("symlinked .scion directory", func(t *testing.T) {
		realDir := filepath.Join(t.TempDir(), "real-scion")
		if err := os.MkdirAll(realDir, 0755); err != nil {
			t.Fatal(err)
		}
		ws := t.TempDir()
		link := filepath.Join(ws, config.DotScion)
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		ident := &config.WorkspaceIdentity{ScionPath: link, ID: relinkOldID, Slug: "proj"}
		if err := writeWorkspaceIdentityNoFollow(ident, "proj", relinkNewID); err == nil {
			t.Fatal("expected a refusal")
		}
		assertAbsent(t, filepath.Join(realDir, "project-id"))
	})
	t.Run("symlinked project-id file", func(t *testing.T) {
		ws := t.TempDir()
		dotScion := filepath.Join(ws, config.DotScion)
		if err := os.MkdirAll(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside-id")
		if err := os.WriteFile(outside, []byte(relinkOldID+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		idFile := filepath.Join(dotScion, "project-id")
		if err := os.Symlink(outside, idFile); err != nil {
			t.Fatal(err)
		}
		ident := &config.WorkspaceIdentity{ScionPath: dotScion, ID: relinkOldID, Slug: "proj"}
		if err := writeWorkspaceIdentityNoFollow(ident, "proj", relinkNewID); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(outside); string(got) != relinkOldID+"\n" {
			t.Fatalf("symlink target written: %q", got)
		}
		info, err := os.Lstat(idFile)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("project-id is not a regular file: %v, %v", info, err)
		}
		if id, err := config.ReadProjectID(dotScion); err != nil || id != relinkNewID {
			t.Fatalf("project-id = %q, %v", id, err)
		}
	})
	t.Run("symlinked marker file", func(t *testing.T) {
		ws := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("keep\n"), 0644); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(ws, config.DotScion)
		if err := os.Symlink(outside, marker); err != nil {
			t.Fatal(err)
		}
		ident := &config.WorkspaceIdentity{ScionPath: marker, Marker: &config.ProjectMarker{ProjectID: relinkOldID, ProjectName: "proj", ProjectSlug: "proj"}, ID: relinkOldID, Slug: "proj"}
		if err := writeWorkspaceIdentityNoFollow(ident, "proj", relinkNewID); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(outside); string(got) != "keep\n" {
			t.Fatalf("symlink target written: %q", got)
		}
		m, err := config.ReadProjectMarker(marker)
		if err != nil || m.ProjectID != relinkNewID {
			t.Fatalf("marker = %+v, %v", m, err)
		}
	})
}

// TestRelink_MarkerSyncFailureAfterRenameKeepsMove: when the marker rename
// lands but syncing its directory fails, the marker counts as rewritten:
// the move is kept, the marker names the current ID, and the next start does
// not resume against an empty directory.
func TestRelink_MarkerSyncFailureAfterRenameKeepsMove(t *testing.T) {
	failSync := func(t *testing.T) {
		prev := relinkMarkerDirSync
		relinkMarkerDirSync = func(*os.File) error { return errors.New("sync refused") }
		t.Cleanup(func() { relinkMarkerDirSync = prev })
	}
	for _, copyPath := range []bool{false, true} {
		name := "rename"
		if copyPath {
			name = "copy"
		}
		t.Run(name, func(t *testing.T) {
			f := newRelinkFixture(t)
			if copyPath {
				forceCopyPath(t)
			}
			failSync(t)
			if err := f.relink(""); err != nil {
				t.Fatalf("expected the re-link to stand, got %v", err)
			}
			if got := f.markerID(t); got != relinkNewID {
				t.Fatalf("marker = %q, want %q", got, relinkNewID)
			}
			assertState(t, f.newRoot)
			assertAbsent(t, f.oldRoot)
			if err := verifySharedProjectIdentity(f.root, relinkNewID); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("interrupted-move recovery", func(t *testing.T) {
		f := newRelinkFixture(t)
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		failSync(t)
		if err := f.relink(""); err != nil {
			t.Fatalf("expected the marker rewrite to stand, got %v", err)
		}
		if got := f.markerID(t); got != relinkNewID {
			t.Fatalf("marker = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
	})
}

// TestRelink_RenameSyncFailure: a failed sync after the directory rename is
// undone and reported as unchanged; when the undo also fails, the error says
// the directory was moved, and the next start finishes the re-link.
func TestRelink_RenameSyncFailure(t *testing.T) {
	failSyncOnce := func(t *testing.T) {
		prev := relinkSyncDir
		calls := 0
		relinkSyncDir = func(dir string) error {
			calls++
			if calls == 1 {
				return errors.New("sync refused")
			}
			return prev(dir)
		}
		t.Cleanup(func() { relinkSyncDir = prev })
	}
	t.Run("undone", func(t *testing.T) {
		f := newRelinkFixture(t)
		failSyncOnce(t)
		err := f.relink("")
		if err == nil || !strings.Contains(err.Error(), "left unchanged") {
			t.Fatalf("expected an unchanged refusal, got %v", err)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker rewritten to %q", got)
		}
	})
	t.Run("undo fails", func(t *testing.T) {
		f := newRelinkFixture(t)
		failSyncOnce(t)
		prev := relinkRenameDir
		calls := 0
		relinkRenameDir = func(oldpath, newpath string) error {
			calls++
			if calls > 1 {
				return errors.New("undo refused")
			}
			return prev(oldpath, newpath)
		}
		t.Cleanup(func() { relinkRenameDir = prev })
		err := f.relink("")
		var re *projectRelinkError
		if !errors.As(err, &re) || !re.Moved || strings.Contains(err.Error(), "left unchanged") {
			t.Fatalf("expected a moved error, got %v", err)
		}
		assertState(t, f.newRoot)
		assertAbsent(t, f.oldRoot)
		if got := f.markerID(t); got != relinkOldID {
			t.Fatalf("marker = %q", got)
		}
		if err := f.relink(""); err != nil {
			t.Fatal(err)
		}
		if got := f.markerID(t); got != relinkNewID {
			t.Fatalf("marker = %q, want %q", got, relinkNewID)
		}
	})
}

// TestWriteWorkspaceIdentityNoFollow_Confined: the write is confined to the
// workspace even when .scion is replaced by a symlink after the checks, and
// stale temporary files from an interrupted write are removed.
func TestWriteWorkspaceIdentityNoFollow_Confined(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := ".scion swapped for an absolute symlink after the checks"
		if relative {
			name = ".scion swapped for a relative symlink after the checks"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			ws := filepath.Join(parent, "ws")
			outside := filepath.Join(parent, "outside")
			dotScion := filepath.Join(ws, config.DotScion)
			for _, d := range []string{dotScion, outside} {
				if err := os.MkdirAll(d, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.WriteProjectID(dotScion, relinkOldID); err != nil {
				t.Fatal(err)
			}
			linkTarget := outside
			if relative {
				linkTarget = filepath.Join("..", "outside")
			}
			prev := relinkBeforeMarkerWrite
			relinkBeforeMarkerWrite = func() {
				if err := os.Rename(dotScion, filepath.Join(ws, "moved-scion")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(linkTarget, dotScion); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { relinkBeforeMarkerWrite = prev })
			ident := &config.WorkspaceIdentity{ScionPath: dotScion, ID: relinkOldID, Slug: "proj"}
			if err := writeWorkspaceIdentityNoFollow(ident, "proj", relinkNewID); err == nil {
				t.Fatal("expected the write to be refused")
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("write escaped the workspace: %v", entries)
			}
		})
	}
	t.Run("stale temporary files are removed", func(t *testing.T) {
		for _, dirForm := range []bool{false, true} {
			ws := t.TempDir()
			marker := filepath.Join(ws, config.DotScion)
			var ident *config.WorkspaceIdentity
			tempDir := ws
			if dirForm {
				if err := os.MkdirAll(marker, 0755); err != nil {
					t.Fatal(err)
				}
				if err := config.WriteProjectID(marker, relinkOldID); err != nil {
					t.Fatal(err)
				}
				ident = &config.WorkspaceIdentity{ScionPath: marker, ID: relinkOldID, Slug: "proj"}
				tempDir = marker
			} else {
				writeRelinkMarker(t, marker, relinkOldID, "proj")
				ident = &config.WorkspaceIdentity{ScionPath: marker, Marker: &config.ProjectMarker{ProjectID: relinkOldID, ProjectName: "proj", ProjectSlug: "proj"}, ID: relinkOldID, Slug: "proj"}
			}
			old := time.Now().Add(-2 * relinkStaleTempAge)
			stale := filepath.Join(tempDir, relinkTempPrefix+"stale")
			fresh := filepath.Join(tempDir, relinkTempPrefix+"fresh")
			keepDir := filepath.Join(tempDir, relinkTempPrefix+"dir")
			link := filepath.Join(tempDir, relinkTempPrefix+"link")
			linkTarget := filepath.Join(t.TempDir(), "keep")
			if err := os.WriteFile(stale, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(stale, old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fresh, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(keepDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(linkTarget, []byte("keep\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(linkTarget, old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Fatal(err)
			}
			if err := writeWorkspaceIdentityNoFollow(ident, "proj", relinkNewID); err != nil {
				t.Fatal(err)
			}
			assertAbsent(t, stale)
			for _, keep := range []string{fresh, keepDir, link} {
				if _, err := os.Lstat(keep); err != nil {
					t.Fatalf("dirForm=%v: %s was removed: %v", dirForm, filepath.Base(keep), err)
				}
			}
			if got, err := os.ReadFile(linkTarget); err != nil || string(got) != "keep\n" {
				t.Fatalf("dirForm=%v: symlink target changed: %q, %v", dirForm, got, err)
			}
			kept := map[string]bool{filepath.Base(fresh): true, filepath.Base(keepDir): true, filepath.Base(link): true}
			entries, _ := os.ReadDir(tempDir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), relinkTempPrefix) && !kept[e.Name()] {
					t.Fatalf("dirForm=%v: temporary file left: %s", dirForm, e.Name())
				}
			}
		}
	})
}

// TestRelink_ConcurrentStartsInOneWorkspace: two starts of the same
// workspace racing through the re-link end with the directory and the marker
// at the current ID, both succeeding, and nothing lost. The second start is
// launched after the first passed its checks and before it moves; it must
// wait for the first and then find the work done.
func TestRelink_ConcurrentStartsInOneWorkspace(t *testing.T) {
	f := newRelinkFixture(t)
	var once sync.Once
	var errB error
	doneB := make(chan struct{})
	prev := relinkBeforeMove
	relinkBeforeMove = func() {
		once.Do(func() {
			go func() {
				defer close(doneB)
				errB = f.relink("")
			}()
			// With the lock, B waits for it and this times out. Without
			// it, B passes its checks too and then blocks in once.Do
			// until this times out, after which both go on to move the
			// same directory and one of them fails.
			select {
			case <-doneB:
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	t.Cleanup(func() { relinkBeforeMove = prev })

	errA := f.relink("")
	<-doneB
	if errA != nil || errB != nil {
		t.Fatalf("expected both starts to succeed: A=%v B=%v", errA, errB)
	}
	if got := f.markerID(t); got != relinkNewID {
		t.Fatalf("marker = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
	if err := verifySharedProjectIdentity(f.root, relinkNewID); err != nil {
		t.Fatal(err)
	}
}

// TestLockRelinkWorkspace: the lock serializes one workspace, does not block
// another, and drops its entry when released.
func TestLockRelinkWorkspace(t *testing.T) {
	unlockA := lockRelinkWorkspace("/ws/a")
	got := make(chan struct{})
	released := make(chan struct{})
	go func() {
		unlock := lockRelinkWorkspace("/ws/a")
		close(got)
		unlock()
		close(released)
	}()
	unlockB := lockRelinkWorkspace("/ws/b")
	unlockB()
	select {
	case <-got:
		t.Fatal("a second holder of the same workspace lock got it")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-got
	<-released
	relinkWorkspaceLocksMu.Lock()
	n := len(relinkWorkspaceLocks)
	relinkWorkspaceLocksMu.Unlock()
	if n != 0 {
		t.Fatalf("%d lock entries left", n)
	}
}
