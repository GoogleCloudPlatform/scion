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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ptone/scion#2878: deleting a hub-managed project on a broker
// also removes the project's shared-dir storage under project-configs.

func doDeleteProject(t *testing.T, srv *Server, slug, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/projects/" + slug
	if projectID != "" {
		target += "?project_id=" + projectID
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, target, nil))
	return rec
}

// seedSharedDir creates <projectDir's shared-dirs base>/<name>/file and
// returns the shared-dirs base.
func seedSharedDir(t *testing.T, projectDir, name string) string {
	t.Helper()
	base, err := config.GetSharedDirsBasePath(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, name, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still present (stat err=%v)", path, err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was removed: %v", path, err)
	}
}

func TestDeleteProject_MarkerProject_RemovesSharedDirStorage(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	extB, _ := makeHubMarkerProject(t, home, "proj-b", scopeProjB, "dev")
	baseA := seedSharedDir(t, extA, "scratch")
	baseB := seedSharedDir(t, extB, "scratch")
	if filepath.Dir(baseA) != filepath.Dir(extA) {
		t.Fatalf("test setup: shared dirs %q not beside %q", baseA, extA)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertGone(t, baseA)
	assertPresent(t, filepath.Join(baseB, "scratch", "file"))
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-b"))
}

// Without a project_id on the request the storage location cannot be
// derived from trusted input, so it is left alone; the hub-managed directory
// itself is still removed.
func TestDeleteProject_MarkerProject_NoProjectID_KeepsSharedDirStorage(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	baseA := seedSharedDir(t, extA, "scratch")

	rec := doDeleteProject(t, srv, "proj-a", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(baseA, "scratch", "file"))
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

func TestDeleteProject_GitSplitStorage_RemovesSharedDirStorage(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionDir, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	base := seedSharedDir(t, scionDir, "scratch")
	if !filepath.IsAbs(base) || filepath.Base(filepath.Dir(filepath.Dir(base))) != config.ProjectConfigsDir {
		t.Fatalf("test setup: shared dirs %q not under project-configs", base)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, base)
}

// A project directory recording a different project than the one the hub
// asked to delete keeps its shared-dir storage; the hub-managed directory
// itself is still removed, as before.
func TestDeleteProject_ProjectIDMismatch_KeepsSharedDirStorage(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	baseA := seedSharedDir(t, extA, "scratch")

	rec := doDeleteProject(t, srv, "proj-a", scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(baseA, "scratch", "file"))
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

func TestHubManagedProjectSharedDirsBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(globalDir, "projects")

	// Legacy in-project .scion dir without a project-id: storage sits
	// inside the project dir, so there is nothing extra to remove.
	legacy := filepath.Join(projects, "legacy")
	if err := os.MkdirAll(filepath.Join(legacy, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := hubManagedProjectSharedDirsBase(globalDir, legacy, "legacy", scopeProjA); got != "" || err != nil {
		t.Errorf("legacy layout: got %q, %v; want nothing", got, err)
	}

	// Missing project dir.
	if got, err := hubManagedProjectSharedDirsBase(globalDir, filepath.Join(projects, "missing"), "missing", scopeProjA); got != "" || err != nil {
		t.Errorf("missing project: got %q, %v; want nothing", got, err)
	}

	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	projA := filepath.Join(projects, "proj-a")

	// Storage not created yet: nothing to remove.
	if got, err := hubManagedProjectSharedDirsBase(globalDir, projA, "proj-a", scopeProjA); got != "" || err != nil {
		t.Errorf("no storage yet: got %q, %v; want nothing", got, err)
	}

	want := seedSharedDir(t, extA, "scratch")
	if got, err := hubManagedProjectSharedDirsBase(globalDir, projA, "proj-a", scopeProjA); got != want || err != nil {
		t.Errorf("marker project: got %q, %v; want %q", got, err, want)
	}

	// Refused: no project ID, another project ID, a slug that is not the
	// directory's.
	for _, tc := range []struct{ slug, id string }{{"proj-a", ""}, {"proj-a", scopeProjB}, {"proj-b", scopeProjA}} {
		if got, err := hubManagedProjectSharedDirsBase(globalDir, projA, tc.slug, tc.id); got != "" || err == nil {
			t.Errorf("slug %q id %q: got %q, %v; want refusal", tc.slug, tc.id, got, err)
		}
	}
}

// Crafted .scion markers must never
// direct the shared-dir removal anywhere but the requested project's own
// project-configs entry.
func TestDeleteProject_CraftedMarkers_KeepStorageOutsideProject(t *testing.T) {
	short := config.ProjectMarker{ProjectID: scopeProjA}.ShortUUID()
	cases := []struct {
		name string
		// setup writes the crafted project proj-a and returns a file that
		// must survive the delete.
		setup func(t *testing.T, home string) string
		// check, when set, runs extra assertions after the delete.
		check func(t *testing.T, home string)
	}{
		{
			name: "slug pointing at another project-configs entry",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "x/../victim"})
				return seedFile(t, filepath.Join(home, ".scion", "project-configs", "victim__"+short, "shared-dirs", "s", "file"))
			},
		},
		{
			name: "slug with parent pointers leaving project-configs",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "../../elsewhere"})
				return seedFile(t, filepath.Join(home, "elsewhere__"+short, "shared-dirs", "s", "file"))
			},
		},
		{
			name: "slug escaping to an absolute path",
			setup: func(t *testing.T, home string) string {
				outside := t.TempDir()
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: escapeTo(filepath.Join(outside, "x"))})
				return seedFile(t, filepath.Join(outside, "x__"+short, "shared-dirs", "s", "file"))
			},
		},
		{
			name: "symlinked project-configs entry",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a"})
				outside := t.TempDir()
				keep := seedFile(t, filepath.Join(outside, "shared-dirs", "s", "file"))
				symlinkInto(t, outside, filepath.Join(home, ".scion", "project-configs", "proj-a__"+short))
				return keep
			},
		},
		{
			name: "symlinked shared-dirs",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a"})
				outside := t.TempDir()
				keep := seedFile(t, filepath.Join(outside, "s", "file"))
				symlinkInto(t, outside, filepath.Join(home, ".scion", "project-configs", "proj-a__"+short, "shared-dirs"))
				return keep
			},
			check: func(t *testing.T, home string) {
				// The symlink itself is left alone, not just its target.
				link := filepath.Join(home, ".scion", "project-configs", "proj-a__"+short, "shared-dirs")
				info, err := os.Lstat(link)
				if err != nil {
					t.Fatalf("expected shared-dirs symlink to survive: %v", err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("expected shared-dirs to still be a symlink, got mode %v", info.Mode())
				}
			},
		},
		{
			// Marker ID and slug match the request, but the external dir
			// records another project, which moves the storage location
			// the entry resolves to. Only the exact-location check catches
			// this; the disagreeing project is left alone entirely.
			name: "external dir records another project",
			setup: func(t *testing.T, home string) string {
				ext, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
				keep := seedFile(t, filepath.Join(filepath.Dir(ext), "shared-dirs", "s", "file"))
				if err := config.WriteProjectID(ext, scopeProjB); err != nil {
					t.Fatal(err)
				}
				return keep
			},
		},
		{
			name: "marker slug differs from the request",
			setup: func(t *testing.T, home string) string {
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-b"})
				return seedFile(t, filepath.Join(home, ".scion", "project-configs", "proj-b__"+short, "shared-dirs", "s", "file"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			keep := tc.setup(t, home)

			rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			assertPresent(t, keep)
			if tc.check != nil {
				tc.check(t, home)
			}
			assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
		})
	}
}

// Slugs outside the slug grammar are refused before any path is built.
func TestHubManagedProjectSharedDirsBase_RejectsSlugGrammar(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	short := config.ProjectMarker{ProjectID: scopeProjA}.ShortUUID()
	for _, slug := range []string{"proj-a.", `proj\a`, "Proj-A", "proj_a"} {
		writeHubMarker(t, home, slug, config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: slug})
		keep := seedFile(t, filepath.Join(globalDir, "project-configs", slug+"__"+short, "shared-dirs", "s", "file"))
		got, err := hubManagedProjectSharedDirsBase(globalDir, filepath.Join(globalDir, "projects", slug), slug, scopeProjA)
		if got != "" || err == nil {
			t.Errorf("slug %q: got %q, %v; want refusal", slug, got, err)
		}
		assertPresent(t, keep)
	}
}

// A failure to remove the shared-dir storage answers 500 and keeps the
// project's .scion entry, so a delete re-run by an operator (the hub does
// not retry it) can still locate the storage.
func TestDeleteProject_SharedDirRemovalFailure_KeepsMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root")
	}
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	baseA := seedSharedDir(t, extA, "scratch")
	locked := filepath.Join(baseA, "scratch")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-a", ".scion"))

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := doDeleteProject(t, srv, "proj-a", scopeProjA); rec.Code != http.StatusNoContent {
		t.Fatalf("retry: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, baseA)
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

func seedFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// symlinkInto creates link -> target, creating link's parent.
func symlinkInto(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// removeProjectConfigsSubtree refuses any target that is not strictly inside
// project-configs, including one reached through a symlinked entry, and
// removes a real subtree inside it.
func TestRemoveProjectConfigsSubtree(t *testing.T) {
	home := t.TempDir()
	globalDir := filepath.Join(home, ".scion")
	configs := filepath.Join(globalDir, config.ProjectConfigsDir)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(configs, "link__"+shortA())); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		configs,
		filepath.Join(configs, ".."),
		outside,
		victim,
		filepath.Join(configs, "..", "..", filepath.Base(home)),
		filepath.Join(configs, "link__"+shortA(), "victim"),
	} {
		if err := removeProjectConfigsSubtree(globalDir, path); err == nil {
			t.Errorf("%s: expected an error", path)
		}
	}
	for _, p := range []string{configs, victim} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}

	inside := filepath.Join(configs, "proj-a__"+shortA(), config.SharedDirsSubdir)
	if err := os.MkdirAll(filepath.Join(inside, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeProjectConfigsSubtree(globalDir, inside); err != nil {
		t.Fatalf("remove %s: %v", inside, err)
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Errorf("%s still exists (err %v)", inside, err)
	}
}

// When the shared-dir storage location cannot be read, the delete answers
// 500 and keeps the project's .scion entry rather than leaving the storage
// behind; a delete re-run by an operator once it is readable removes both.
func TestDeleteProject_SharedDirStorageUnreadable_KeepsMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root")
	}
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	baseA := seedSharedDir(t, extA, "scratch")
	locked := filepath.Dir(baseA)
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-a", ".scion"))

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := doDeleteProject(t, srv, "proj-a", scopeProjA); rec.Code != http.StatusNoContent {
		t.Fatalf("retry: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, baseA)
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

// A project-configs layout that is wrong rather than unreadable (a regular
// file or a symlink loop in place of a directory) is not reported as
// unreadable: the storage is left alone and the project directory is still
// removed.
func TestDeleteProject_SharedDirLayoutError_NotUnreadable(t *testing.T) {
	cases := []struct {
		name string
		// break replaces path with a non-directory.
		breakPath func(t *testing.T, path string)
	}{
		{"regular file (ENOTDIR)", func(t *testing.T, path string) { seedFile(t, path) }},
		{"symlink loop (ELOOP)", func(t *testing.T, path string) { symlinkInto(t, filepath.Base(path), path) }},
	}
	for _, tc := range cases {
		for _, level := range []string{"project-configs", "entry"} {
			t.Run(tc.name+" at "+level, func(t *testing.T) {
				mgr := &filteringMockManager{}
				srv, home := newScopeTestServer(t, mgr)
				writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "proj-a", ProjectSlug: "proj-a"})
				configs := filepath.Join(home, ".scion", config.ProjectConfigsDir)
				path := configs
				if level == "entry" {
					path = filepath.Join(configs, "proj-a__"+shortA())
					if err := os.MkdirAll(configs, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				tc.breakPath(t, path)
				other := seedFile(t, filepath.Join(home, ".scion", "projects", "proj-b", "keep"))

				rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
				if rec.Code != http.StatusNoContent {
					t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
				}
				assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
				if _, err := os.Lstat(path); err != nil {
					t.Errorf("%s was removed: %v", path, err)
				}
				assertPresent(t, other)
			})
		}
	}
}

// A symlinked project-configs entry is a layout error even when its target
// cannot be read: it is classified before the readability probe, so the
// delete succeeds and leaves the target alone instead of failing.
func TestDeleteProject_SymlinkedEntryToUnreadable_LeftAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root")
	}
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	writeHubMarker(t, home, "proj-a", config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "proj-a", ProjectSlug: "proj-a"})
	target := filepath.Join(t.TempDir(), "elsewhere")
	keep := seedFile(t, filepath.Join(target, config.SharedDirsSubdir, "s", "file"))
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	symlinkInto(t, target, filepath.Join(home, ".scion", config.ProjectConfigsDir, "proj-a__"+shortA()))

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	assertPresent(t, keep)
}
