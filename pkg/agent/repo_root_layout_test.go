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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const layoutTestProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"

// nfsProjectStorageSettings returns settings with server.project_storage on
// nfs, the share mounted under mountRoot.
func nfsProjectStorageSettings(mountRoot string) *config.VersionedSettings {
	return &config.VersionedSettings{Server: &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{
		Backend: "nfs",
		NFS:     &config.V1NFSConfig{MountRoot: mountRoot, Shares: []config.V1NFSShare{{ID: "share-a", Server: "10.0.0.2", Export: "/vol-a"}}},
	}}}
}

// The shared candidate root is the Layout's workspace of the project
// (<ProjectRoot>/workspace), the same path the nfs workspace backend gives,
// for the single block and the deprecated block it is synthesized from.
func TestSharedLayoutWorkspaceRoot_IsTheBackendWorkspace(t *testing.T) {
	mountRoot := t.TempDir()
	want := filepath.Join(mountRoot, "share-a", "projects", layoutTestProjectID, "workspace")

	ps := nfsProjectStorageSettings(mountRoot)
	legacy := &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: ps.Server.ProjectStorage.WorkspaceStorage()}}
	for name, settings := range map[string]*config.VersionedSettings{"project_storage": ps, "workspace_storage": legacy} {
		t.Run(name, func(t *testing.T) {
			resolved, err := runtime.SelectWorkspaceBackend(settings.Server.EffectiveWorkspaceStorage(), store.SharingModeSharedPlain).
				Resolve(runtime.ResolveInput{ProjectID: layoutTestProjectID, Mode: store.SharingModeSharedPlain})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.HostPath != want {
				t.Fatalf("fixture: backend workspace = %q, want %q", resolved.HostPath, want)
			}
			got, err := sharedLayoutWorkspaceRoot(settings, layoutTestProjectID, resolved.HostPath)
			if err != nil || got != want {
				t.Fatalf("sharedLayoutWorkspaceRoot = (%q, %v), want (%q, nil)", got, err, want)
			}
		})
	}
}

// A backend path the Layout does not give, a Layout that is not shared, an
// invalid project ID, or unusable storage settings refuse the start.
func TestSharedLayoutWorkspaceRoot_RefusesDisagreement(t *testing.T) {
	mountRoot := t.TempDir()
	settings := nfsProjectStorageSettings(mountRoot)
	ws := filepath.Join(mountRoot, "share-a", "projects", layoutTestProjectID, "workspace")
	noShares := nfsProjectStorageSettings(mountRoot)
	noShares.Server.ProjectStorage.NFS.Shares = nil
	cases := []struct {
		name      string
		settings  *config.VersionedSettings
		projectID string
		workspace string
		wantErr   string
	}{
		{"other project's workspace", settings, layoutTestProjectID,
			filepath.Join(mountRoot, "share-a", "projects", "11111111-2222-3333-4444-555555555555", "workspace"), "places it at " + ws},
		{"workspace outside the tree", settings, layoutTestProjectID, filepath.Join(t.TempDir(), "workspace"), "refusing to start"},
		{"local layout", nil, layoutTestProjectID, ws, `project storage layout is "local"`},
		{"invalid project id", settings, "../x", ws, "invalid project ID"},
		{"no project id", settings, "", ws, "invalid project ID"},
		{"unusable settings", noShares, layoutTestProjectID, ws, "no NFS shares configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sharedLayoutWorkspaceRoot(tc.settings, tc.projectID, tc.workspace)
			if err == nil || got != "" {
				t.Fatalf("sharedLayoutWorkspaceRoot = (%q, %v), want a refusal", got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "refusing to start") {
				t.Fatalf("error %q should contain %q and say it refuses", err, tc.wantErr)
			}
		})
	}
}

// With the shared candidate, a shared workspace that is its own checkout is
// the repo root, one without .git is a plain mount, and one whose .git names
// a foreign common dir is refused: git verifies, it never picks the root. The
// broker-local project dir plays no part.
func TestDetectRepoRootFrom_SharedLayoutWorkspace(t *testing.T) {
	mountRoot := t.TempDir()
	settings := nfsProjectStorageSettings(mountRoot)
	projectDir := filepath.Join(t.TempDir(), "home", config.GlobalDir, config.ProjectsDir, "p", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(mountRoot, "share-a", "projects", layoutTestProjectID, "workspace")

	t.Run("plain", func(t *testing.T) {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		root, err := sharedLayoutWorkspaceRoot(settings, layoutTestProjectID, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := detectRepoRootFrom(ws, projectDir, root); err != nil || got != "" {
			t.Fatalf("detectRepoRootFrom = (%q, %v), want (\"\", nil)", got, err)
		}
	})
	t.Run("own checkout", func(t *testing.T) {
		mkGitRepo(t, ws)
		root, err := sharedLayoutWorkspaceRoot(settings, layoutTestProjectID, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := detectRepoRootFrom(ws, projectDir, root); err != nil || got != ws {
			t.Fatalf("detectRepoRootFrom = (%q, %v), want (%q, nil)", got, err, ws)
		}
	})
	t.Run("foreign common dir", func(t *testing.T) {
		foreign := mkGitRepo(t, filepath.Join(t.TempDir(), "foreign"))
		setCommonDir(t, ws, filepath.Join(foreign, ".git"), filepath.Join(foreign, ".git"))
		root, err := sharedLayoutWorkspaceRoot(settings, layoutTestProjectID, ws)
		if err != nil {
			t.Fatal(err)
		}
		got, err := detectRepoRootFrom(ws, projectDir, root)
		if err == nil || got != "" || !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("detectRepoRootFrom = (%q, %v), want a refusal", got, err)
		}
	})
}

// Local backend unchanged: on the local layout the candidate root is the project's
// own root, unchanged.
func TestLocalLayoutWorkspaceRoot_Unchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if got := localLayoutWorkspaceRoot(dir); got != dir {
		t.Fatalf("localLayoutWorkspaceRoot(%q) = %q, want it unchanged", dir, got)
	}
	if got := localLayoutWorkspaceRoot(""); got != "" {
		t.Fatalf("localLayoutWorkspaceRoot(\"\") = %q, want \"\"", got)
	}
}

// composeSharedRepoRoot falls back to the shared layout's workspace and keeps
// the record intersection of composeRepoRoot: a missing or equal record uses
// the shared root, a different or unreadable record refuses, a refusal from
// the layout is returned unchanged whatever the record, and an explicit
// workspace derives no root. The non-shared path (composeRepoRoot) never
// consults the shared layout.
func TestComposeSharedRepoRoot(t *testing.T) {
	mountRoot := t.TempDir()
	settings := nfsProjectStorageSettings(mountRoot)
	projectDir := filepath.Join(t.TempDir(), "home", config.GlobalDir, config.ProjectsDir, "p", config.DotScion)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(mountRoot, "share-a", "projects", layoutTestProjectID, "workspace")
	mkGitRepo(t, ws)
	other := filepath.Join(mountRoot, "share-a", "projects", "11111111-2222-3333-4444-555555555555", "workspace")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	_, layoutErr := sharedLayoutWorkspaceRoot(settings, layoutTestProjectID, other)
	if layoutErr == nil {
		t.Fatal("fixture: the other project's workspace should be refused by the layout")
	}

	missing := repoRootRecordState{status: recordMissing}
	equal := repoRootRecordState{status: recordPresent, root: ws}
	different := repoRootRecordState{status: recordPresent, root: filepath.Join(t.TempDir(), "local-root")}
	unreadable := repoRootRecordState{status: recordUnreadable}
	cases := []struct {
		name      string
		workspace string
		explicit  bool
		rec       repoRootRecordState
		wantRoot  string
		wantErr   string
	}{
		{"record missing: shared root", ws, false, missing, ws, ""},
		{"record equal: shared root", ws, false, equal, ws, ""},
		{"record different: refused", ws, false, different, "", "does not match"},
		{"record unreadable: refused", ws, false, unreadable, "", "could not be read"},
		{"layout refusal, record missing: unchanged", other, false, missing, "", layoutErr.Error()},
		{"layout refusal, record present: unchanged", other, false, equal, "", layoutErr.Error()},
		{"explicit workspace: no root", ws, true, equal, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeSharedRepoRoot("a1", "", tc.rec, tc.workspace, projectDir, layoutTestProjectID, tc.explicit, settings)
			if tc.wantErr == "" {
				if err != nil || got != tc.wantRoot {
					t.Fatalf("composeSharedRepoRoot = (%q, %v), want (%q, nil)", got, err, tc.wantRoot)
				}
				return
			}
			if err == nil || got != "" {
				t.Fatalf("composeSharedRepoRoot = (%q, %v), want a refusal", got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("layout refusal is the layout's own error", func(t *testing.T) {
		_, err := composeSharedRepoRoot("a1", "", missing, other, projectDir, layoutTestProjectID, false, settings)
		if err == nil || err.Error() != layoutErr.Error() {
			t.Fatalf("composeSharedRepoRoot error = %v, want exactly %v", err, layoutErr)
		}
	})
	t.Run("non-shared path does not consult the shared layout", func(t *testing.T) {
		// On the local layout the shared recomposition refuses, while
		// composeRepoRoot derives through the local layout and does not.
		if _, err := composeSharedRepoRoot("a1", "", missing, ws, projectDir, layoutTestProjectID, false, nil); err == nil ||
			!strings.Contains(err.Error(), `project storage layout is "local"`) {
			t.Fatalf("composeSharedRepoRoot on the local layout = %v, want the local-layout refusal", err)
		}
		if _, err := composeRepoRoot("a1", "", missing, ws, projectDir, false, nil); err != nil &&
			strings.Contains(err.Error(), "project storage layout") {
			t.Fatalf("composeRepoRoot consulted the shared layout: %v", err)
		}
	})
}
