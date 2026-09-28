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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProjects_EmptyHome(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

func TestDiscoverProjects_GlobalOnly(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	globalDir := filepath.Join(tmpHome, ".scion")
	_ = os.MkdirAll(filepath.Join(globalDir, "agents"), 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].Type != ProjectTypeGlobal {
		t.Errorf("expected global type, got %s", projects[0].Type)
	}
	if projects[0].Name != "global" {
		t.Errorf("expected name 'global', got %s", projects[0].Name)
	}
	if projects[0].Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", projects[0].Status)
	}
}

// TestDiscoverProjects_JSONHasNoGroveIDField is a negative regression test
// for the removed ProjectInfo.GroveID field: it proves the JSON emitted for
// a discovered project with a project ID set contains "project_id" and
// never contains "grove_id". Unmarshals into []map[string]any (rather than
// back into ProjectInfo) so the assertion is driven by the actual JSON keys
// on the wire, not by the Go struct's tags, which is the only way this
// catches the field coming back.
func TestDiscoverProjects_JSONHasNoGroveIDField(t *testing.T) {
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_HUB_GROVE_ID", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			defer func() { _ = os.Setenv(e, val) }()
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	globalDir := filepath.Join(tmpHome, ".scion")
	_ = os.MkdirAll(filepath.Join(globalDir, "agents"), 0755)

	const projectID = "abcd1234-0000-0000-0000-000000000000"
	settingsContent := "project_id: " + projectID + "\n"
	_ = os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].ProjectID != projectID {
		t.Fatalf("expected ProjectID %q on the Go struct, got %q", projectID, projects[0].ProjectID)
	}

	data, err := json.Marshal(projects)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected 1 entry in the raw JSON, got %d", len(raw))
	}

	entry := raw[0]
	if got, ok := entry["project_id"]; !ok || got != projectID {
		t.Errorf("expected JSON field \"project_id\" = %q, got %v (present: %v)", projectID, got, ok)
	}
	if _, ok := entry["grove_id"]; ok {
		t.Errorf("JSON output must not contain a \"grove_id\" key, got entry: %v", entry)
	}
}

func TestDiscoverProjects_ExternalProject(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			defer func() { _ = os.Setenv(e, val) }()
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Create global dir
	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create an external project config
	projectConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__abcd1234", ".scion")
	_ = os.MkdirAll(filepath.Join(projectConfigDir, "agents", "agent1"), 0755)

	// Create a workspace directory with a marker file
	workspace := filepath.Join(tmpHome, "projects", "myproject")
	_ = os.MkdirAll(workspace, 0755)

	// Write marker file
	marker := &ProjectMarker{
		ProjectID:   "abcd1234-0000-0000-0000-000000000000",
		ProjectName: "myproject",
		ProjectSlug: "myproject",
	}
	_ = WriteProjectMarker(filepath.Join(workspace, DotScion), marker)

	// Write settings with workspace_path
	settingsContent := "workspace_path: " + workspace + "\ngrove_id: abcd1234-0000-0000-0000-000000000000\n"
	_ = os.WriteFile(filepath.Join(projectConfigDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should find global + external
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}

	ext := projects[1]
	if ext.Type != ProjectTypeExternal {
		t.Errorf("expected external type, got %s", ext.Type)
	}
	if ext.Name != "myproject" {
		t.Errorf("expected name 'myproject', got %s", ext.Name)
	}
	if ext.Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", ext.Status)
	}
	if ext.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", ext.AgentCount)
	}
	if ext.WorkspacePath != workspace {
		t.Errorf("expected workspace %s, got %s", workspace, ext.WorkspacePath)
	}
}

func TestDiscoverProjects_OrphanedExternal(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			defer func() { _ = os.Setenv(e, val) }()
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create external project config pointing to a non-existent workspace
	projectConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "gone-project__deadbeef", ".scion")
	_ = os.MkdirAll(filepath.Join(projectConfigDir, "agents"), 0755)

	settingsContent := "workspace_path: /nonexistent/workspace\n"
	_ = os.WriteFile(filepath.Join(projectConfigDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the external project
	var ext *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeExternal {
			ext = &projects[i]
			break
		}
	}
	if ext == nil {
		t.Fatal("expected to find external project")
	}
	if ext.Status != ProjectStatusOrphaned {
		t.Errorf("expected status orphaned, got %s", ext.Status)
	}
}

func TestFindOrphanedProjectConfigs(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create one orphaned project
	orphanedDir := filepath.Join(tmpHome, ".scion", "project-configs", "orphan__12345678", ".scion")
	_ = os.MkdirAll(filepath.Join(orphanedDir, "agents"), 0755)
	_ = os.WriteFile(filepath.Join(orphanedDir, "settings.yaml"), []byte("workspace_path: /does/not/exist\n"), 0644)

	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned, got %d", len(orphaned))
	}
	if orphaned[0].Name != "orphan" {
		t.Errorf("expected name 'orphan', got %s", orphaned[0].Name)
	}
}

func TestRemoveProjectConfig(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	configDir := filepath.Join(tmpHome, ".scion", "project-configs", "test__aabbccdd", ".scion")
	_ = os.MkdirAll(configDir, 0755)
	_ = os.WriteFile(filepath.Join(configDir, "settings.yaml"), []byte(""), 0644)

	if err := RemoveProjectConfig(configDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parentDir := filepath.Dir(configDir)
	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Errorf("expected directory to be removed, but it still exists")
	}
}

func TestRemoveProjectConfig_ThroughMigratedSymlink(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Build the pre-migration layout directly under the legacy root, then
	// run the real migrator. MigrateLegacyGlobalLayout symlinks each entry
	// it moves individually; the legacy root itself (grove-configs/) stays a
	// real directory, so configPath is reached through a per-entry symlink,
	// not a symlinked root.
	scionDir := filepath.Join(tmpHome, ".scion")
	entryDir := filepath.Join(scionDir, "grove-configs", "test__aabbccdd", ".scion")
	_ = os.MkdirAll(entryDir, 0755)
	_ = os.WriteFile(filepath.Join(entryDir, "settings.yaml"), []byte(""), 0644)
	MigrateLegacyGlobalLayout(scionDir, &fakeReporter{})

	realDir := filepath.Join(scionDir, "project-configs", "test__aabbccdd")
	if _, err := os.Stat(realDir); err != nil {
		t.Fatalf("migration did not create the canonical dir: %v", err)
	}
	entryLink := filepath.Join(scionDir, "grove-configs", "test__aabbccdd")
	if info, err := os.Lstat(entryLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to be a symlink after migration, got %v, err %v", entryLink, info, err)
	}

	configDir := filepath.Join(entryLink, ".scion")
	if err := RemoveProjectConfig(configDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(realDir); !os.IsNotExist(err) {
		t.Errorf("expected the canonical directory to be removed, but it still exists")
	}
	if _, err := os.Lstat(entryLink); !os.IsNotExist(err) {
		t.Errorf("expected the leftover legacy symlink to be removed, but it still exists (err=%v)", err)
	}
}

// TestRemoveProjectConfig_SiblingSymlinkNotDeleted guards against a
// hand-made or corrupted symlink that points at a different project's
// config entry: only the link itself must be removed, never the sibling it
// points at. Scion never creates a link like this (MigrateLegacyGlobalLayout
// only links an old entry name straight to its own same-named canonical
// entry), but this path is reachable from a project-delete request, so a
// crafted or corrupted link must not turn into a wrong-target recursive
// delete.
func TestRemoveProjectConfig_SiblingSymlinkNotDeleted(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs")
	sibling := filepath.Join(projectConfigsDir, "b__22222222")
	if err := os.MkdirAll(filepath.Join(sibling, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, ".scion", "settings.yaml"), []byte("keep-me"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projectConfigsDir, "a__11111111")
	if err := os.Symlink(sibling, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the link itself to be removed, but it still exists (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(sibling, ".scion", "settings.yaml"))
	if err != nil {
		t.Fatalf("sibling project config was deleted: %v", err)
	}
	if string(data) != "keep-me" {
		t.Errorf("sibling project config content changed: got %q", data)
	}
}

// TestRemoveProjectConfig_DotSymlinkNotDeleted guards against a symlink that
// points at project-configs/ itself (directly, or via "."): only the link
// must be removed, never the whole project-configs/ tree.
func TestRemoveProjectConfig_DotSymlinkNotDeleted(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs")
	other := filepath.Join(projectConfigsDir, "b__22222222", ".scion")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projectConfigsDir, "a__11111111")
	if err := os.Symlink(".", link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the link itself to be removed, but it still exists (err=%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("project-configs/ was deleted: %v", err)
	}
}

// TestRemoveProjectConfig_RefusesRootItself guards against a configPath that
// resolves to project-configs/ itself, with no entry name at all: it must be
// refused, never removed.
func TestRemoveProjectConfig_RefusesRootItself(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs")
	if err := os.MkdirAll(projectConfigsDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(projectConfigsDir); err == nil {
		t.Fatal("expected an error removing project-configs/ itself, got nil")
	}
	if _, err := os.Stat(projectConfigsDir); err != nil {
		t.Fatalf("project-configs/ was removed: %v", err)
	}
}

// TestRemoveProjectConfig_MissingEntryThroughSymlinkedHome guards against a
// false ErrPermission when $HOME (or ~/.scion) is itself behind a symlink
// and the requested entry simply doesn't exist: the prefix check resolves
// the canonical side but a missing entry can't resolve on the other side, so
// without an explicit existence check first the two never compare equal.
func TestRemoveProjectConfig_MissingEntryThroughSymlinkedHome(t *testing.T) {
	realHome := filepath.Join(t.TempDir(), "real-home")
	// project-configs/ itself must exist (and so resolve) for this to
	// reproduce the bug: the failure is a mismatch between the resolved
	// canonical root and the unresolved literal path of a missing entry,
	// not a missing root.
	if err := os.MkdirAll(filepath.Join(realHome, ".scion", "project-configs"), 0755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", linkedHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	missing := filepath.Join(linkedHome, ".scion", "project-configs", "gone__33333333", ".scion")
	if err := RemoveProjectConfig(missing); err != nil {
		t.Fatalf("expected nil for a missing entry, got: %v", err)
	}
}

// TestRemoveProjectConfig_ExistingEntryThroughSymlinkedHome removes a real,
// existing entry (not a symlinked one) when $HOME is itself behind a
// symlink. This is the case TestRemoveProjectConfig_MissingEntryThroughSymlinkedHome
// does not cover: that test returns at the missing-entry check before the
// prefix comparison ever runs, so a mutation that compares the unresolved
// project-configs path instead of the resolved root survives unnoticed. This
// test exercises the comparison itself, on a path that must actually match.
func TestRemoveProjectConfig_ExistingEntryThroughSymlinkedHome(t *testing.T) {
	realHome := filepath.Join(t.TempDir(), "real-home")
	entryDir := filepath.Join(realHome, ".scion", "project-configs", "test__aabbccdd", ".scion")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", linkedHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	configPath := filepath.Join(linkedHome, ".scion", "project-configs", "test__aabbccdd", ".scion")
	if err := RemoveProjectConfig(configPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(realHome, ".scion", "project-configs", "test__aabbccdd")); !os.IsNotExist(err) {
		t.Errorf("expected the entry to be removed, but it still exists (err=%v)", err)
	}
}

// TestRemoveProjectConfig_LinkInRootToOutsideTargetUnlinked guards a link
// that lives directly under project-configs/ but points somewhere outside
// ~/.scion entirely: only the link is removed, and its target — a real
// project directory unrelated to this one — must survive untouched.
func TestRemoveProjectConfig_LinkInRootToOutsideTargetUnlinked(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	outsideTarget := filepath.Join(t.TempDir(), "unrelated-target")
	if err := os.MkdirAll(filepath.Join(outsideTarget, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideTarget, ".scion", "settings.yaml"), []byte("keep-me"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmpHome, ".scion", "project-configs", "c__33333333")
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the link itself to be removed, but it still exists (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(outsideTarget, ".scion", "settings.yaml"))
	if err != nil {
		t.Fatalf("outside target was deleted: %v", err)
	}
	if string(data) != "keep-me" {
		t.Errorf("outside target content changed: got %q", data)
	}
}

// TestRemoveProjectConfig_DanglingLinkInRootThroughSymlinkedHome guards a
// dangling link living directly under project-configs/ when $HOME is itself
// behind a symlink: the link is unlinked, not refused with a permission
// error (which would leave it behind and, in the hub's caller, log a
// spurious warning).
func TestRemoveProjectConfig_DanglingLinkInRootThroughSymlinkedHome(t *testing.T) {
	realHome := filepath.Join(t.TempDir(), "real-home")
	projectConfigsDir := filepath.Join(realHome, ".scion", "project-configs")
	if err := os.MkdirAll(projectConfigsDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projectConfigsDir, "a__11111111")
	if err := os.Symlink(filepath.Join(realHome, "nonexistent-target"), link); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", linkedHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := RemoveProjectConfig(filepath.Join(linkedHome, ".scion", "project-configs", "a__11111111", ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the dangling link to be removed, but it still exists (err=%v)", err)
	}
}

// TestRemoveProjectConfig_LinkOutsideScionRefused guards a link whose own
// location is outside ~/.scion entirely (not inside project-configs/, and
// not in a directory alongside it): refused outright, link kept, regardless
// of what it points at.
func TestRemoveProjectConfig_LinkOutsideScionRefused(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	target := filepath.Join(tmpHome, ".scion", "project-configs", "b__22222222")
	if err := os.MkdirAll(filepath.Join(target, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	link := filepath.Join(outsideDir, "x")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err == nil {
		t.Fatal("expected an error for a link living outside ~/.scion, got nil")
	}

	if _, err := os.Lstat(link); err != nil {
		t.Errorf("expected the link to survive being refused, but it's gone (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".scion")); err != nil {
		t.Errorf("target was deleted despite being refused: %v", err)
	}
}

// TestRemoveProjectConfig_RefusesSymlinkedRootItself guards a configPath
// that names project-configs/ itself when that path happens to be a
// symlink: refused, exactly like the real-directory root-itself case,
// never touched (neither unlinked nor recursed into).
func TestRemoveProjectConfig_RefusesSymlinkedRootItself(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	realTarget := filepath.Join(t.TempDir(), "real-project-configs")
	if err := os.MkdirAll(realTarget, 0755); err != nil {
		t.Fatal(err)
	}
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	projectConfigsDir := filepath.Join(scionDir, "project-configs")
	if err := os.Symlink(realTarget, projectConfigsDir); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(projectConfigsDir); err == nil {
		t.Fatal("expected an error removing a symlinked project-configs/ itself, got nil")
	}

	if _, err := os.Lstat(projectConfigsDir); err != nil {
		t.Errorf("expected the project-configs symlink to survive being refused, but it's gone (err=%v)", err)
	}
	if _, err := os.Stat(realTarget); err != nil {
		t.Errorf("the symlink's target was deleted despite being refused: %v", err)
	}
}

// TestRemoveProjectConfig_SiblingLinkInLegacyDirNotDeleted guards the one
// branch that recursively deletes a link's target: a link living in the
// legacy directory but pointing at a *different* entry under
// project-configs/ must only be unlinked, never followed into a recursive
// delete of that other project's config.
func TestRemoveProjectConfig_SiblingLinkInLegacyDirNotDeleted(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	scionDir := filepath.Join(tmpHome, ".scion")
	sibling := filepath.Join(scionDir, "project-configs", "b__22222222")
	if err := os.MkdirAll(filepath.Join(sibling, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, ".scion", "settings.yaml"), []byte("keep-me"), 0644); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(scionDir, legacyProjectConfigsDirName)
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(legacyDir, "a__11111111")
	if err := os.Symlink(sibling, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the link itself to be removed, but it still exists (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(sibling, ".scion", "settings.yaml"))
	if err != nil {
		t.Fatalf("sibling project config was deleted: %v", err)
	}
	if string(data) != "keep-me" {
		t.Errorf("sibling project config content changed: got %q", data)
	}
}

// TestRemoveProjectConfig_SameNameLinkInLegacyDirToOutsideTargetUnlinked
// guards the same branch against a same-named link whose target lives
// outside ~/.scion entirely: the direct-child-of-root check must still
// refuse the recursive delete, leaving only the link removed.
func TestRemoveProjectConfig_SameNameLinkInLegacyDirToOutsideTargetUnlinked(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	outsideTarget := filepath.Join(t.TempDir(), "x__33333333")
	if err := os.MkdirAll(filepath.Join(outsideTarget, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideTarget, ".scion", "settings.yaml"), []byte("keep-me"), 0644); err != nil {
		t.Fatal(err)
	}
	scionDir := filepath.Join(tmpHome, ".scion")
	legacyDir := filepath.Join(scionDir, legacyProjectConfigsDirName)
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(legacyDir, "x__33333333")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected the link itself to be removed, but it still exists (err=%v)", err)
	}
	data, err := os.ReadFile(filepath.Join(outsideTarget, ".scion", "settings.yaml"))
	if err != nil {
		t.Fatalf("outside target was deleted: %v", err)
	}
	if string(data) != "keep-me" {
		t.Errorf("outside target content changed: got %q", data)
	}
}

// TestRemoveProjectConfig_LinkInOtherScionSubdirRefused guards against
// widening the recursive-delete branch to any directory beside
// project-configs/: a link living in an unrelated ~/.scion subdirectory
// (not the legacy directory) must be refused outright, never unlinked or
// recursed into.
func TestRemoveProjectConfig_LinkInOtherScionSubdirRefused(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	scionDir := filepath.Join(tmpHome, ".scion")
	target := filepath.Join(scionDir, "project-configs", "b__22222222")
	if err := os.MkdirAll(filepath.Join(target, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(scionDir, "templates")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(otherDir, "b__22222222")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := RemoveProjectConfig(filepath.Join(link, ".scion")); err == nil {
		t.Fatal("expected an error for a link living in an unrelated ~/.scion subdirectory, got nil")
	}

	if _, err := os.Lstat(link); err != nil {
		t.Errorf("expected the link to survive being refused, but it's gone (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".scion")); err != nil {
		t.Errorf("target was deleted despite being refused: %v", err)
	}
}

// TestRemoveProjectConfig_RelativeConfigPath guards against a relative
// configPath being refused outright: the canonical root is always absolute
// (built from os.UserHomeDir()), so every internal comparison must convert a
// relative path to absolute before comparing it, or a relative entry would
// never match. This is covered by projectkeys.ResolvePathForCompare
// resolving to absolute internally, not by a separate conversion in this
// function, so this test also pins that behaviour.
func TestRemoveProjectConfig_RelativeConfigPath(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs")
	entryDir := filepath.Join(projectConfigsDir, "rel__1", ".scion")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectConfigsDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	// Given relative to the cwd set above.
	if err := RemoveProjectConfig("rel__1/.scion"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectConfigsDir, "rel__1")); !os.IsNotExist(err) {
		t.Errorf("expected rel__1 to be removed, but it still exists")
	}
}

// TestRemoveProjectConfig_CleansRawPathWithDotDot guards against a raw,
// uncleaned configPath (containing "..") being judged lexically but acted on
// physically. "a" is a symlink to a *subdirectory* of an outside directory,
// so "a/.." physically lands back in that outside directory (not in
// project-configs/, and not back at "a" — a symlinked path component is
// substituted in place before ".." is applied, it doesn't remember where the
// traversal started), where a same-named symlink also lives. Lexically,
// filepath.Clean collapses "project-configs/a/../b__1" to
// "project-configs/b__1", which was never created — the two views name
// different things unless the raw path is cleaned before use, and without
// that the outside symlink gets unlinked through a path that never
// mentioned its real location.
func TestRemoveProjectConfig_CleansRawPathWithDotDot(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs")
	if err := os.MkdirAll(projectConfigsDir, 0755); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	sub := filepath.Join(outsideDir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, filepath.Join(projectConfigsDir, "a")); err != nil {
		t.Fatal(err)
	}
	outsideLinkTarget := filepath.Join(t.TempDir(), "somewhere-else")
	if err := os.MkdirAll(outsideLinkTarget, 0755); err != nil {
		t.Fatal(err)
	}
	outsideLink := filepath.Join(outsideDir, "b__1")
	if err := os.Symlink(outsideLinkTarget, outsideLink); err != nil {
		t.Fatal(err)
	}

	// Built by concatenation, not filepath.Join, so the ".." survives
	// uncleaned into the call.
	raw := projectConfigsDir + "/a/../b__1"

	if err := RemoveProjectConfig(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Lstat(outsideLink); err != nil {
		t.Errorf("expected the outside symlink, reachable only through the uncleaned \"..\", to survive untouched, but it's gone (err=%v)", err)
	}
}

func TestRemoveProjectConfig_SafetyCheck(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Try to remove something outside project-configs — should fail
	outsideDir := filepath.Join(tmpHome, "projects", "important")
	_ = os.MkdirAll(outsideDir, 0755)

	err := RemoveProjectConfig(outsideDir)
	if err == nil {
		t.Error("expected error when removing path outside project-configs")
	}
}

func TestReconnectProject(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Create external project config
	configDir := filepath.Join(tmpHome, ".scion", "project-configs", "proj__11223344", ".scion")
	_ = os.MkdirAll(configDir, 0755)
	_ = os.WriteFile(filepath.Join(configDir, "settings.yaml"), []byte("workspace_path: /old/path\n"), 0644)

	newPath := filepath.Join(tmpHome, "new-workspace")
	_ = os.MkdirAll(newPath, 0755)

	if err := ReconnectProject(configDir, newPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify settings updated
	settings, err := LoadSettings(configDir)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if settings.WorkspacePath != newPath {
		t.Errorf("expected workspace_path %s, got %s", newPath, settings.WorkspacePath)
	}
}

func TestCountAgents(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents")
	_ = os.MkdirAll(filepath.Join(agentsDir, "agent-a"), 0755)
	_ = os.MkdirAll(filepath.Join(agentsDir, "agent-b"), 0755)
	_ = os.MkdirAll(filepath.Join(agentsDir, ".hidden"), 0755)

	count := countAgents(agentsDir)
	if count != 2 {
		t.Errorf("expected 2 agents, got %d", count)
	}
}

func TestCountAgents_NonExistentDir(t *testing.T) {
	count := countAgents("/nonexistent/agents")
	if count != 0 {
		t.Errorf("expected 0 agents, got %d", count)
	}
}

func TestListAgentNames(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents")
	_ = os.MkdirAll(filepath.Join(agentsDir, "agent-a"), 0755)
	_ = os.MkdirAll(filepath.Join(agentsDir, "agent-b"), 0755)
	_ = os.MkdirAll(filepath.Join(agentsDir, ".hidden"), 0755)

	names := ListAgentNames(agentsDir)
	if len(names) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(names))
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["agent-a"] || !nameSet["agent-b"] {
		t.Errorf("expected agent-a and agent-b, got %v", names)
	}
}

func TestListAgentNames_NonExistentDir(t *testing.T) {
	names := ListAgentNames("/nonexistent/agents")
	if names != nil {
		t.Errorf("expected nil, got %v", names)
	}
}

func TestProjectInfo_AgentsDir(t *testing.T) {
	tests := []struct {
		name     string
		project  ProjectInfo
		expected string
	}{
		{
			name: "external project",
			project: ProjectInfo{
				Type:       ProjectTypeExternal,
				ConfigPath: "/home/user/.scion/project-configs/proj__abc123/.scion",
			},
			expected: "/home/user/.scion/project-configs/proj__abc123/.scion/agents",
		},
		{
			name: "git project",
			project: ProjectInfo{
				Type:       ProjectTypeGit,
				ConfigPath: "/home/user/.scion/project-configs/repo__def456/.scion",
			},
			expected: "/home/user/.scion/project-configs/repo__def456/.scion/agents",
		},
		{
			name: "global project",
			project: ProjectInfo{
				Type:       ProjectTypeGlobal,
				ConfigPath: "/home/user/.scion",
			},
			expected: "/home/user/.scion/agents",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.project.AgentsDir()
			if got != tt.expected {
				t.Errorf("AgentsDir() = %s, want %s", got, tt.expected)
			}
		})
	}
}

func TestDiscoverProjects_StaleExternalAfterMarkerRecreate(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			defer func() { _ = os.Setenv(e, val) }()
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	workspace := filepath.Join(tmpHome, "projects", "myproject")
	_ = os.MkdirAll(workspace, 0755)

	// Simulate the old project-config (from a previous init)
	oldConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__aaaaaaaa", ".scion")
	_ = os.MkdirAll(filepath.Join(oldConfigDir, "agents"), 0755)
	_ = os.WriteFile(filepath.Join(oldConfigDir, "settings.yaml"),
		[]byte("workspace_path: "+workspace+"\ngrove_id: aaaaaaaa-0000-0000-0000-000000000000\n"), 0644)

	// Simulate new project-config (from re-init after marker was deleted)
	newConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__bbbbbbbb", ".scion")
	_ = os.MkdirAll(filepath.Join(newConfigDir, "agents"), 0755)
	_ = os.WriteFile(filepath.Join(newConfigDir, "settings.yaml"),
		[]byte("workspace_path: "+workspace+"\ngrove_id: bbbbbbbb-0000-0000-0000-000000000000\n"), 0644)

	// Workspace marker now points to the new project-config
	marker := &ProjectMarker{
		ProjectID:   "bbbbbbbb-0000-0000-0000-000000000000",
		ProjectName: "myproject",
		ProjectSlug: "myproject",
	}
	_ = WriteProjectMarker(filepath.Join(workspace, DotScion), marker)

	// The old config should be orphaned because the marker resolves to the new config
	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned project-config, got %d", len(orphaned))
	}
	if orphaned[0].Name != "myproject" {
		t.Errorf("expected orphaned name 'myproject', got %s", orphaned[0].Name)
	}
	// The orphaned one should be the old config
	if orphaned[0].ConfigPath != oldConfigDir {
		t.Errorf("expected orphaned config path %s, got %s", oldConfigDir, orphaned[0].ConfigPath)
	}
}

func TestDiscoverProjects_ShadowProjectNotOrphaned(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a workspace directory with a valid shadow marker
	workspaceDir := filepath.Join(tmpHome, "remote-proj")
	_ = os.MkdirAll(workspaceDir, 0755)
	marker := &ProjectMarker{
		ProjectID:   "aabb1122-0000-0000-0000-000000000000",
		ProjectName: "remote-proj",
		ProjectSlug: "remote-proj",
		Type:        "shadow",
	}
	_ = WriteProjectMarker(filepath.Join(workspaceDir, DotScion), marker)

	// Create a shadow project config directory with versioned settings
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "remote-proj__aabb1122")
	scionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(scionDir, 0755)

	// Write versioned settings with project_type: shadow, hub config, and workspace_path
	settingsContent := "schema_version: \"1\"\nproject_type: shadow\nworkspace_path: " + workspaceDir + "\nhub:\n  enabled: true\n  endpoint: https://hub.example.com\n  project_id: aabb1122-0000-0000-0000-000000000000\n"
	_ = os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var shadow *ProjectInfo
	for i := range projects {
		if projects[i].Name == "remote-proj" {
			shadow = &projects[i]
			break
		}
	}
	if shadow == nil {
		t.Fatal("expected to find shadow project")
	}
	if shadow.Type != ProjectTypeShadow {
		t.Errorf("expected type %q, got %q", ProjectTypeShadow, shadow.Type)
	}
	if shadow.Status != ProjectStatusOK {
		t.Errorf("expected status %q for shadow project (should not be orphaned), got %q", ProjectStatusOK, shadow.Status)
	}
	if shadow.ProjectID != "aabb1122-0000-0000-0000-000000000000" {
		t.Errorf("expected project ID from hub config, got %q", shadow.ProjectID)
	}
}

func TestDiscoverProjects_GitProjectExternal(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project external directory (agents only, no .scion subdir)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "myrepo__aabb1122")
	agentsDir := filepath.Join(projectDir, "agents", "worker1", "home")
	_ = os.MkdirAll(agentsDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeGit {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project")
	}
	if gitProject.Name != "myrepo" {
		t.Errorf("expected name 'myrepo', got %s", gitProject.Name)
	}
	if gitProject.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", gitProject.AgentCount)
	}
	if gitProject.Status != ProjectStatusOK {
		t.Errorf("expected status ok for git project with agents, got %s", gitProject.Status)
	}
}

func TestDiscoverProjects_GitProjectExternalEmptyAgents(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project external directory with an empty agents dir (no .scion subdir)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "leftover__deadbeef")
	_ = os.MkdirAll(filepath.Join(projectDir, "agents"), 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeGit {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project")
	}
	if gitProject.Status != ProjectStatusOrphaned {
		t.Errorf("expected orphaned status for git project with empty agents dir, got %s", gitProject.Status)
	}
}

func TestDiscoverProjects_GitProjectWithExternalConfig(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project in the new layout: .scion/ (config + agents)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "newrepo__ccdd1122")
	scionDir := filepath.Join(projectDir, ".scion")
	agentsDir := filepath.Join(scionDir, "agents", "worker1", "home")
	_ = os.MkdirAll(scionDir, 0755)
	_ = os.MkdirAll(agentsDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Name == "newrepo" {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project with external config")
	}
	if gitProject.Type != ProjectTypeGit {
		t.Errorf("expected ProjectTypeGit, got %s", gitProject.Type)
	}
	if gitProject.Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", gitProject.Status)
	}
	if gitProject.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", gitProject.AgentCount)
	}
	if gitProject.ConfigPath != scionDir {
		t.Errorf("expected ConfigPath %q, got %q", scionDir, gitProject.ConfigPath)
	}
	// AgentsDir() should point to .scion/agents/ inside the project config dir
	wantAgentsDir := filepath.Join(scionDir, "agents")
	if got := gitProject.AgentsDir(); got != wantAgentsDir {
		t.Errorf("AgentsDir() = %q, want %q", got, wantAgentsDir)
	}
}

func TestDiscoverProjects_GitProjectWithExternalConfigUsesWorkspaceMarkerProjectID(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("Setenv HOME failed: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME failed: %v", err)
		}
	}()

	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}

	for _, envName := range []string{"SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(envName); ok {
			if err := os.Unsetenv(envName); err != nil {
				t.Fatalf("Unsetenv %s failed: %v", envName, err)
			}
			defer func(name, v string) {
				if err := os.Setenv(name, v); err != nil {
					t.Fatalf("restore %s failed: %v", name, err)
				}
			}(envName, val)
		}
	}

	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "newrepo__ccdd1122")
	scionDir := filepath.Join(projectDir, ".scion")
	agentsDir := filepath.Join(scionDir, "agents", "worker1", "home")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}

	workspaceDir := filepath.Join(tmpHome, ".scion", "projects", "newrepo")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := WriteWorkspaceMarker(workspaceDir, "3c619ec9-517e-4321-8c6a-4757f6a95607", "newrepo", "newrepo"); err != nil {
		t.Fatalf("WriteWorkspaceMarker failed: %v", err)
	}

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Name == "newrepo" {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project with external config")
	}
	if gitProject.ProjectID != "3c619ec9-517e-4321-8c6a-4757f6a95607" {
		t.Fatalf("ProjectID = %q, want %q", gitProject.ProjectID, "3c619ec9-517e-4321-8c6a-4757f6a95607")
	}
	if gitProject.WorkspacePath != workspaceDir {
		t.Fatalf("WorkspacePath = %q, want %q", gitProject.WorkspacePath, workspaceDir)
	}
}

func TestDiscoverProjects_ProjectConfigNoScionNoAgents(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a project-config directory with no .scion and no agents subdir
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "empty-leftover__aabb1122")
	_ = os.MkdirAll(projectDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found *ProjectInfo
	for i := range projects {
		if projects[i].Name == "empty-leftover" {
			found = &projects[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected to find orphaned project-config dir with no .scion and no agents")
	}
	if found.Status != ProjectStatusOrphaned {
		t.Errorf("expected orphaned status, got %s", found.Status)
	}
}

func TestFindOrphanedProjectConfigs_IncludesEmptyGitProjects(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	_ = os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create leftover project-config with empty agents dir (typical test residue)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "ws-test__abcd1234")
	_ = os.MkdirAll(filepath.Join(projectDir, "agents"), 0755)

	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned, got %d", len(orphaned))
	}
	if orphaned[0].Name != "ws-test" {
		t.Errorf("expected name 'ws-test', got %s", orphaned[0].Name)
	}
}
