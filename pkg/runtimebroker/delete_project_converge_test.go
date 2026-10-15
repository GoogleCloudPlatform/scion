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

// Project cleanup converges (design §7, CleanupProject convergence): the
// project-configs entry is removed even when the project directory is
// already gone, and the delete succeeds only once both are gone.

// TestDeleteProject_ConfigsRemovedWhenDirGone: with the project directory
// already gone, the project's project-configs entry is removed by its
// canonical name; entries of other projects stay.
func TestDeleteProject_ConfigsRemovedWhenDirGone(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	sameSlug := seedProjectConfigsEntry(t, home, "proj-a", scopeProjB)
	dirB := seedProjectConfigsEntry(t, home, "proj-b", scopeProjB)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
	assertPresent(t, filepath.Join(sameSlug, "launch-marker"))
	assertPresent(t, filepath.Join(dirB, "launch-marker"))
}

// TestDeleteProject_OrphanConfigsKept: with the project directory gone,
// an entry that cannot be shown to be this project's stays.
func TestDeleteProject_OrphanConfigsKept(t *testing.T) {
	t.Run("no project ID", func(t *testing.T) {
		srv, home := newScopeTestServer(t, &filteringMockManager{})
		dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
		rec := doDeleteProject(t, srv, "proj-a", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
		}
		assertPresent(t, filepath.Join(dirA, "launch-marker"))
	})
	t.Run("entry records another project with the same short ID", func(t *testing.T) {
		srv, home := newScopeTestServer(t, &filteringMockManager{})
		dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
		if err := config.WriteProjectID(filepath.Join(dirA, config.DotScion), samePrefixMarkerID); err != nil {
			t.Fatal(err)
		}
		rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
		}
		assertPresent(t, filepath.Join(dirA, "launch-marker"))
	})
	t.Run("entry is a symlink", func(t *testing.T) {
		srv, home := newScopeTestServer(t, &filteringMockManager{})
		elsewhere := t.TempDir()
		if err := os.WriteFile(filepath.Join(elsewhere, "keep"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		entry := projectConfigsEntry(home, "proj-a", scopeProjA)
		if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, entry); err != nil {
			t.Fatal(err)
		}
		rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
		}
		assertPresent(t, filepath.Join(elsewhere, "keep"))
	})
}

// TestDeleteProject_NoContentOnlyWhenBothGone: while the entry remains
// and whether it is this project's cannot be read, the delete fails;
// once that is resolved, a repeated delete removes it and succeeds.
func TestDeleteProject_NoContentOnlyWhenBothGone(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	// A project-id entry that cannot be read as a file.
	unreadable := filepath.Join(dirA, config.DotScion, "project-id")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code < 300 {
		t.Fatalf("expected a failure while the entry remains, got %d", rec.Code)
	}
	assertPresent(t, filepath.Join(dirA, "launch-marker"))

	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	rec = doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on the repeat, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
}

// TestDeleteProject_RepeatConverges: a delete repeated after full success
// is still a success and removes nothing else.
func TestDeleteProject_RepeatConverges(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	dirB := seedProjectConfigsEntry(t, home, "proj-b", scopeProjB)
	for i := range 2 {
		rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete %d: expected 204, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertGone(t, dirA)
	assertPresent(t, filepath.Join(dirB, "launch-marker"))
}

// TestDeleteProject_ConfigsRemovalFailureIsError: when the project-configs
// entry cannot be removed after the project directory was, the delete
// fails instead of reporting success, and a repeat converges once the
// entry can be removed. Permissions do not stop root, so the test needs
// an unprivileged user.
func TestDeleteProject_ConfigsRemovalFailureIsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict root")
	}
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	locked := filepath.Join(dirA, config.DotScion, "agents", "dev")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code < 300 {
		t.Fatalf("expected a failure, got %d", rec.Code)
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertPresent(t, filepath.Join(dirA, "launch-marker"))

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	rec = doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on the repeat, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
}
