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
	"errors"
	"net/http"
	"path/filepath"
	"testing"
)

// failProjectConfigsRemoval makes srv's project-configs removal fail until
// the returned function restores it.
func failProjectConfigsRemoval(srv *Server) (restore func()) {
	srv.mu.Lock()
	srv.projectConfigsRemove = func(string, string) error { return errors.New("removal failed (test)") }
	srv.mu.Unlock()
	return func() {
		srv.mu.Lock()
		srv.projectConfigsRemove = nil
		srv.mu.Unlock()
	}
}

// TestDeleteProject_DirGoneRemovalFailureIsError: with the project
// directory already gone, a failure to remove the project-configs entry
// fails the delete and keeps the entry; a repeated delete removes it and
// answers 204. Does not depend on the user the test runs as.
func TestDeleteProject_DirGoneRemovalFailureIsError(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)

	restore := failProjectConfigsRemoval(srv)
	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code < 300 {
		t.Fatalf("expected a failure, got %d", rec.Code)
	}
	assertPresent(t, filepath.Join(dirA, "launch-marker"))

	restore()
	rec = doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on the repeat, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
}

// TestDeleteProject_PostRemovalFailureIsError: when the project-configs
// entry cannot be removed after the project directory was, the delete
// fails instead of reporting success, and a repeat converges. Does not
// depend on the user the test runs as.
func TestDeleteProject_PostRemovalFailureIsError(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)

	restore := failProjectConfigsRemoval(srv)
	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code < 300 {
		t.Fatalf("expected a failure, got %d", rec.Code)
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertPresent(t, filepath.Join(dirA, "launch-marker"))

	restore()
	rec = doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on the repeat, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
}
