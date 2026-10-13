// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the files-owner record in the host state file
// (ptone/scion#4771): an owner that cannot be determined keeps the files on
// every path, a recorded value only names a run (it never allows more than
// the run the hub is deleting), and the transitional legacy agent-info.json
// value can only keep files.

// plantHostState writes content as the agent's host state file directly,
// as a file committed to the repository would be (the hub-native layout
// with a .scion directory). "symlink" plants a symlink to another file.
func plantHostState(t *testing.T, scionDir, name, content string) {
	t.Helper()
	dir := filepath.Join(scionDir, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, agent.HostStateFile)
	if content == "symlink" {
		other := filepath.Join(t.TempDir(), "other.json")
		if err := os.WriteFile(other, []byte(`{"runId":"run-old"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, path); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A stale record naming the deleting run does not by itself allow the
// delete: with a newer run's container, or a newer run's start in flight,
// no file is deleted.
//
// The "newer run's container" case is decided by resolveDeleteTarget's
// run filter before the record is read (the record plays no part); the
// in-flight cases reach the files-owner check, where the record names the
// deleting run (no other owner) and the in-flight start keeps the files.
func TestDeleteAgent_StaleOwnerWithNewerRun_DeletesNoFiles(t *testing.T) {
	t.Run("newer run's container", func(t *testing.T) {
		mgr := &filteringMockManager{}
		srv, home := newScopeTestServer(t, mgr)
		scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
		infoB := recordRun(t, scionB, "dev", "run-old")
		mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "run-new") {
			t.Fatalf("expected a 404 naming run-new, got %d: %s", rec.Code, rec.Body.String())
		}
		if mgr.DeleteCalls() != 0 {
			t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
		}
		assertUntouched(t, scionB, "dev", infoB)
	})
	t.Run("newer run's start in flight", func(t *testing.T) {
		mgr := &cleanupRecordingManager{}
		srv, home := newCleanupTestServer(t, mgr)
		scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
		infoB := recordRun(t, scionB, "dev", "run-old")
		lr := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() {})
		lr.RunID = "run-new"
		srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		if mgr.DeleteCalls() != 0 {
			t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
		}
		assertCleanupCalls(t, mgr.cleanupCalls())
		assertUntouched(t, scionB, "dev", infoB)
	})
	t.Run("deleting run's container and a newer run's start in flight", func(t *testing.T) {
		mgr := &filteringMockManager{}
		srv, home := newScopeTestServer(t, mgr)
		scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
		infoB := recordRun(t, scionB, "dev", "run-old")
		mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}
		lr := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() {})
		lr.RunID = "run-new"
		srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)

		rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
		}
		if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "cid-old" {
			t.Fatalf("got %d deletes, last %q; want the requested run's cid-old", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
		}
		if mgr.LastDeleteFiles() {
			t.Error("the delete removed files while a newer run's start is in flight")
		}
		assertUntouched(t, scionB, "dev", infoB)
	})
}

// The host state file's content decides only between "this run", "another
// run" and "unknown". A file that cannot be read, or a value that is not a
// valid run ID, is unknown and keeps the files; a planted file naming a
// foreign run keeps them too; one naming the deleting run deletes no more
// than the hub-run match allows. With the deleting run's container present
// the container is removed either way.
func TestDeleteAgent_HostStateContent(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantKept      bool
	}{
		{"corrupt file", `{"runId":`, true},
		{"invalid run ID", `{"runId":"a,b"}`, true},
		{"symlinked file", "symlink", true},
		{"planted foreign run ID", `{"runId":"run-foreign"}`, true},
		{"planted deleting run ID", `{"runId":"run-old"}`, false},
	} {
		t.Run(tc.name+", container", func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			plantHostState(t, scionB, "dev", tc.content)
			mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "cid-old" {
				t.Fatalf("got %d deletes, last %q; want the requested run's cid-old", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
			}
			if got := mgr.LastDeleteFiles(); got == tc.wantKept {
				t.Errorf("deleteFiles = %v, want %v", got, !tc.wantKept)
			}
			if tc.wantKept {
				assertUntouched(t, scionB, "dev", infoB)
			}
		})
		t.Run(tc.name+", files only", func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, home := newCleanupTestServer(t, mgr)
			scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			plantHostState(t, scionB, "dev", tc.content)

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
			if tc.wantKept {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
				}
				if mgr.DeleteCalls() != 0 {
					t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
				}
				assertCleanupCalls(t, mgr.cleanupCalls())
				assertUntouched(t, scionB, "dev", infoB)
				return
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if !mgr.LastDeleteFiles() || mgr.LastDeleteProjectPath() != scionB {
				t.Errorf("got files=%v path %q, want the files deleted in %q", mgr.LastDeleteFiles(), mgr.LastDeleteProjectPath(), scionB)
			}
		})
	}
}

// The transitional legacy fallback: an agent whose runId is still only in
// agent-info.json (recorded by an older broker) keeps its files when a
// delete names another run, and is deleted by a delete naming its run.
// Once the host state file records a run, the legacy value is ignored.
func TestDeleteAgent_LegacyAgentInfoRunID(t *testing.T) {
	for _, tc := range []struct {
		name, legacyRun, hostRun string
		wantKept                 bool
	}{
		{"legacy run differs", "run-new", "", true},
		{"legacy run matches", "run-old", "", false},
		{"host state run matches, legacy run differs", "run-new", "run-old", false},
		{"host state run differs, legacy run matches", "run-old", "run-new", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, home := newCleanupTestServer(t, mgr)
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			recordRun(t, scionB, "dev", tc.hostRun)
			infoB := writeAgentInfo(t, scionB, "dev", tc.legacyRun)

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
			if tc.wantKept {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
				}
				if mgr.DeleteCalls() != 0 {
					t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
				}
				assertUntouched(t, scionB, "dev", infoB)
				return
			}
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if !mgr.LastDeleteFiles() || mgr.LastDeleteProjectPath() != scionB {
				t.Errorf("got files=%v path %q, want the files deleted in %q", mgr.LastDeleteFiles(), mgr.LastDeleteProjectPath(), scionB)
			}
		})
	}
}

// A failed synchronous create keeps the files when their owner is unknown:
// an unreadable host state file, or a project with no trusted root (a
// symlinked .scion).
func TestSyncCreateFailure_FilesOwnerUnknown_KeepsFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		symlink bool
	}{
		{"corrupt host state file", false},
		{"symlinked .scion", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)
			requestPath := projectPath
			if tc.symlink {
				linked := filepath.Join(t.TempDir(), "linked")
				if err := os.MkdirAll(linked, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(projectPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(projectPath, filepath.Join(linked, ".scion")); err != nil {
					t.Fatal(err)
				}
				requestPath = filepath.Join(linked, ".scion")
			}
			var started atomic.Bool
			mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
				started.Store(true)
				writeAgentFiles(t, agentDir, "provisioned")
				if !tc.symlink {
					plantHostState(t, projectPath, "same-name", `{"runId":`)
				}
				return nil, errors.New("container start failed")
			}
			body := fmt.Sprintf(`{"id":"agent-a-id","name":"same-name","projectPath":%q,"runId":"run-a","config":{"task":"t"}}`, requestPath)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code < 400 {
				t.Fatalf("expected the create to fail, got %d: %s", w.Code, w.Body.String())
			}
			if !started.Load() {
				t.Fatal("the create did not reach Manager.Start; the test does not exercise the failure cleanup")
			}
			if _, err := os.Stat(agentDir); err != nil {
				t.Errorf("the failure cleanup removed files whose owner is unknown: %v", err)
			}
		})
	}
}

// The async launch's failure cleanup keeps the files when their owner is
// unknown.
func TestCleanupAbortedLaunch_FilesOwnerUnknown_KeepsFiles(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentDir := filepath.Join(projectDir, "agents", "agent-x")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantHostState(t, projectDir, "agent-x", `{"runId":"a,b"}`)
	if err := writeLaunchMarker(projectDir, false, "agent-x", "L1"); err != nil {
		t.Fatal(err)
	}
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-x", ProjectPath: projectDir, RunID: "run-a"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-x"},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(agentDir); err != nil {
		t.Errorf("the launch cleanup removed files whose owner is unknown: %v", err)
	}
}
