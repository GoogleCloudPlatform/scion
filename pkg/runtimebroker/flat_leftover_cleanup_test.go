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
	"strings"
	"testing"
)

// Leftover per-agent object cleanup on a flat instance (ptone/scion#3274):
// owner-scoped, only for a slug the instance's record holds, never while a
// newer run is live, and a failure fails the delete.

func ownedCleanupCalls(f *partitionFixture) []string {
	var out []string
	for _, c := range f.d.recorded() {
		if strings.HasPrefix(c, "cleanupOwned:") || strings.HasPrefix(c, "cleanupAgentResources:") {
			out = append(out, c)
		}
	}
	return out
}

func TestFlatLeftoverCleanup_SlugNotOwnedSkips(t *testing.T) {
	f := newPartitionFixture(t)
	// B's helper on A, and the unlabeled historical object: A's record holds
	// neither slug, so no leftover cleanup runs.
	for _, path := range []string{"/api/v1/agents/helper?projectId=proj-1", "/api/v1/agents/old?projectId=proj-1"} {
		w := serveFlat(f.a.srv, http.MethodDelete, path+"&deleteFiles=true", "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404: %s", path, w.Code, w.Body.String())
		}
	}
	if calls := ownedCleanupCalls(f); len(calls) != 0 {
		t.Fatalf("leftover cleanup ran for slugs A does not own: %v", calls)
	}
}

func TestFlatLeftoverCleanup_OwnedSlugIsOwnerScoped(t *testing.T) {
	f := newRunFenceFixture(t, OwnershipStateDeleted, true)
	// Only the old run remains live: its container is gone, so the delete
	// finds nothing and cleans up leftovers, scoped to A's owner label.
	w := serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1&runId="+fenceOldRun+"&deleteFiles=true", "")
	if w.Code >= 300 && w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := "cleanupOwned:proj-1/worker/" + f.a.identity.RuntimeBrokerID
	if calls := ownedCleanupCalls(f); len(calls) != 1 || calls[0] != want {
		t.Fatalf("leftover cleanup calls = %v, want [%s] (owner-scoped, never unscoped)", calls, want)
	}
}

func TestFlatLeftoverCleanup_RunFencedWithOtherLiveRunSkips(t *testing.T) {
	f := newRunFenceFixture(t, OwnershipStateCreated, true)
	serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1&runId="+fenceOldRun+"&deleteFiles=true", "")
	if calls := ownedCleanupCalls(f); len(calls) != 0 {
		t.Fatalf("leftover cleanup ran while a newer run is live: %v", calls)
	}
}

func TestFlatLeftoverCleanup_FailureFailsTheDelete(t *testing.T) {
	f := newRunFenceFixture(t, OwnershipStateDeleted, true)
	f.d.mu.Lock()
	f.d.ownedCleanupErr = errors.New("list secrets: forbidden")
	f.d.mu.Unlock()
	w := serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1&runId="+fenceOldRun+"&deleteFiles=true", "")
	if w.Code < 500 {
		t.Fatalf("status %d, want a delete failure: %s", w.Code, w.Body.String())
	}
}
