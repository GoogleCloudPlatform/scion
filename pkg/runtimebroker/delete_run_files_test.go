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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#2675: agent files, worktree and leftover per-agent
// objects are addressed by name, so a delete or failure cleanup naming one
// run must leave alone those of a newer run that reuses the name. The
// owning run is the runId recorded in agent-info.json.

// recordRun rewrites the agent's agent-info.json with runID as its owning
// run ("" writes a legacy file with no runId).
func recordRun(t *testing.T, scionDir, name, runID string) string {
	t.Helper()
	home := config.GetAgentHomePath(scionDir, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","phase":"running"`
	if runID != "" {
		body += `,"runId":"` + runID + `"`
	}
	body += "}"
	info := filepath.Join(home, "agent-info.json")
	if err := os.WriteFile(info, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return info
}

// A container of the requested run is deleted, but the files, recorded as a
// newer run's, are not: no file deletion and no soft-delete mark.
func TestDeleteAgent_FilesOfOtherRun_DeletesEntryOnly(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	infoB := recordRun(t, scionB, "dev", "run-new")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "cid-old" {
		t.Fatalf("got %d deletes, last %q; want the requested run's cid-old", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteFiles() {
		t.Error("the delete removed files recorded as another run's")
	}
	assertUntouched(t, scionB, "dev", infoB)
}

// Files only, recorded as another run's: 404 and nothing touched, not even
// leftover per-agent runtime objects (they are the other run's too).
func TestDeleteAgent_FilesOfOtherRun_FileOnly404NoSideEffects(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	infoB := recordRun(t, scionB, "dev", "run-new")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
	assertUntouched(t, scionB, "dev", infoB)
}

// The file-only and container flows proceed as before when the recorded run
// matches, when none is recorded (legacy files), and when the delete names
// no run.
func TestDeleteAgent_FilesRunCheck_ProceedsAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name, diskRun, query string
		container            bool
	}{
		{"file-only, recorded run matches", "run-old", "&runId=run-old", false},
		{"file-only, legacy files", "", "&runId=run-old", false},
		{"file-only, no runId sent", "run-new", "", false},
		{"container, recorded run matches", "run-old", "&runId=run-old", true},
		{"container, legacy files", "", "&runId=run-old", true},
		{"container, no runId sent", "run-new", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			recordRun(t, scionB, "dev", tc.diskRun)
			if tc.container {
				mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionB), "run-old")}
			}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+tc.query+"&deleteFiles=true")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if !mgr.LastDeleteFiles() || mgr.LastDeleteProjectPath() != scionB {
				t.Errorf("got files=%v path %q, want the files deleted in %q", mgr.LastDeleteFiles(), mgr.LastDeleteProjectPath(), scionB)
			}
		})
	}
}

// A start of another run still in flight on this broker owns the name even
// before it has recorded its run on disk (still provisioning): a delete
// naming a different run leaves the files alone. A start of the same run is
// cancelled and its files deleted, as before.
func TestDeleteAgent_InFlightStartOfOtherRun_KeepsFiles(t *testing.T) {
	for _, tc := range []struct {
		name, inflightRun string
		wantCode          int
	}{
		{"other run in flight", "run-new", http.StatusNotFound},
		{"same run in flight", "run-old", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, home := newCleanupTestServer(t, mgr)
			scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			lr := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() {})
			lr.RunID = tc.inflightRun
			srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantCode == http.StatusNotFound {
				if mgr.DeleteCalls() != 0 {
					t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
				}
				assertCleanupCalls(t, mgr.cleanupCalls())
				assertUntouched(t, scionB, "dev", infoB)
			} else if !mgr.LastDeleteFiles() {
				t.Error("the delete of the in-flight start's own run did not delete its files")
			}
		})
	}
}

// No container and no files yet, but a start of another run is in flight:
// the not-found delete leaves per-agent runtime objects alone, since that
// start may be creating them under this name (review N2). Without one in
// flight, or for the same run, the leftover cleanup runs as before.
func TestDeleteAgent_NotFoundWithOtherRunInFlight_SkipsLeftoverCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, inflightRun string
		wantCleanup       bool
	}{
		{"other run in flight", "run-new", false},
		{"same run in flight", "run-old", true},
		{"nothing in flight", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, _ := newCleanupTestServer(t, mgr)
			if tc.inflightRun != "" {
				lr := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() {})
				lr.RunID = tc.inflightRun
				srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)
			}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&deleteFiles=true")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
			}
			if tc.wantCleanup {
				assertCleanupCalls(t, mgr.cleanupCalls(), cleanupCall{"dev", scopeProjB})
			} else {
				assertCleanupCalls(t, mgr.cleanupCalls())
			}
		})
	}
}

// The in-flight check before the not-found cleanup also matches a launch
// registered under the slugified name, the name the cleanup acts on
// (review round 2, Nit 2).
func TestDeleteAgent_NotFoundInFlightCheckUsesSlug(t *testing.T) {
	mgr := &cleanupRecordingManager{}
	srv, _ := newCleanupTestServer(t, mgr)
	lr := newLaunchRecord("sync-1", "dev-agent", "create", "", time.Time{}, func() {})
	lr.RunID = "run-new"
	srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev-agent"}, lr)

	rec := doDelete(t, srv, "Dev-Agent", "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	assertCleanupCalls(t, mgr.cleanupCalls())
}

// A provision-only create's owner (review round 2, Nit 4): a late delete
// naming another run (the predecessor's) touches nothing; the agent's own
// delete, which names no run until its first start, deletes as before.
func TestDeleteAgent_ProvisionOwner(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		wantCode    int
	}{
		{"predecessor's run", "&runId=run-ghost", http.StatusNotFound},
		{"no run (the agent's own delete)", "", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &cleanupRecordingManager{}
			srv, home := newCleanupTestServer(t, mgr)
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
			infoB := recordRun(t, scionB, "dev", agent.ProvisionOwnerPrefix+"abc")

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+tc.query+allDeleteParams)
			if rec.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantCode == http.StatusNotFound {
				if mgr.DeleteCalls() != 0 {
					t.Errorf("expected no DeleteTarget call, got %d", mgr.DeleteCalls())
				}
				assertCleanupCalls(t, mgr.cleanupCalls())
				assertUntouched(t, scionB, "dev", infoB)
				return
			}
			if !mgr.LastDeleteFiles() || mgr.LastDeleteProjectPath() != scionB {
				t.Errorf("got files=%v path %q, want the files deleted in %q", mgr.LastDeleteFiles(), mgr.LastDeleteProjectPath(), scionB)
			}
		})
	}
}

func TestLaunchRegistry_OtherRunInFlight(t *testing.T) {
	key := launchKey{ProjectID: "p1", Slug: "a"}
	for _, tc := range []struct {
		name, recRun, deleteRun string
		register                bool
		want                    bool
	}{
		{"other run", "r2", "r1", true, true},
		{"same run", "r1", "r1", true, false},
		{"launch without run", "", "r1", true, false},
		{"delete without run", "r2", "", true, false},
		{"nothing registered", "", "r1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLaunchRegistry()
			if tc.register {
				rec := newLaunchRecord("L", "a", "create", "", time.Time{}, func() {})
				rec.RunID = tc.recRun
				r.Begin(key, rec)
			}
			if got := r.OtherRunInFlight(key, tc.deleteRun); got != tc.want {
				t.Errorf("OtherRunInFlight = %v, want %v", got, tc.want)
			}
		})
	}
	var nilReg *launchRegistry
	if nilReg.OtherRunInFlight(key, "r1") {
		t.Error("a nil registry reports a launch in flight")
	}
}

// --- Real files: a real agent.Manager removes a real worktree. ---

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeGitHubProject creates a hub-managed git project
// ~/.scion/projects/<slug> whose agent's workspace is a git worktree on
// branch <agent>, with the agent's files recorded as owned by runID. It
// returns the project's .scion dir, the repo root, the agent dir and the
// worktree path.
func makeGitHubProject(t *testing.T, home, slug, projectID, name, runID string) (scionDir, repo, agentDir, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".scion/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-q", "-m", "init")
	scionDir = filepath.Join(repo, ".scion")
	agentDir = filepath.Join(scionDir, "agents", name)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scionDir, projectID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(agentDir, "workspace")
	runGit(t, repo, "worktree", "add", "-q", "-b", name, worktree)
	if err := os.WriteFile(filepath.Join(worktree, "work.txt"), []byte("uncommitted work"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordRun(t, scionDir, name, runID)
	return scionDir, repo, agentDir, worktree
}

// labelFilterRuntime is a runtime.MockRuntime over a fixed entry list that
// honours List's label filter and records deletes.
func labelFilterRuntime(entries *[]api.AgentInfo, deleted *[]string) *runtime.MockRuntime {
	var mu sync.Mutex
	return &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			mu.Lock()
			defer mu.Unlock()
			var out []api.AgentInfo
			for _, e := range *entries {
				ok := true
				for k, v := range filter {
					if e.Labels[k] != v {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, e)
				}
			}
			return out, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			mu.Lock()
			defer mu.Unlock()
			*deleted = append(*deleted, ref.ID)
			return nil
		},
	}
}

func newRealFilesServer(t *testing.T, rt *runtime.MockRuntime) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	return New(cfg, agent.NewManager(rt), rt), home
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func worktreeRegistered(t *testing.T, repo, path string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		real = filepath.Dir(path)
	}
	return strings.Contains(string(out), "worktree "+path+"\n") ||
		strings.Contains(string(out), "worktree "+filepath.Join(real, filepath.Base(path))+"\n")
}

// A stale delete after a same-name recreate leaves the new run's agent dir,
// worktree (with its uncommitted work and registration) and branch on
// disk; a delete naming the new run removes the agent dir and worktree.
// Both with the old run's container still present (the runtime entry alone
// is removed) and with no container at all (the new run still
// provisioning, the file-only flow).
func TestDeleteAgent_StaleRunAfterRecreate_RealFiles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		oldEntry    bool
		wantCode    int
		wantDeleted []string
	}{
		{"old run's container still present", true, http.StatusNoContent, []string{"cid-old"}},
		{"no container (file-only)", false, http.StatusNotFound, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// util skips worktree pruning when SCION_HOST_UID is set (inside
			// an agent container); clear it so the host behaviour is tested
			// wherever this runs.
			t.Setenv("SCION_HOST_UID", "")
			var entries []api.AgentInfo
			var deleted []string
			srv, home := newRealFilesServer(t, labelFilterRuntime(&entries, &deleted))
			scionDir, repo, agentDir, worktree := makeGitHubProject(t, home, "proj-b", scopeProjB, "dev", "run-new")
			if tc.oldEntry {
				entries = []api.AgentInfo{withRun(labelled("dev", "cid-old", scopeProjB, scionDir), "run-old")}
			}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old"+allDeleteParams)
			if rec.Code != tc.wantCode {
				t.Fatalf("stale delete: expected %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if fmt.Sprint(deleted) != fmt.Sprint(tc.wantDeleted) {
				t.Errorf("runtime deletes = %v, want %v", deleted, tc.wantDeleted)
			}
			if _, err := os.Stat(filepath.Join(worktree, "work.txt")); err != nil {
				t.Fatalf("the new run's worktree was removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err != nil {
				t.Fatalf("the new run's agent dir was removed: %v", err)
			}
			if !branchExists(t, repo, "dev") {
				t.Fatal("the new run's branch was removed")
			}
			if !worktreeRegistered(t, repo, worktree) {
				t.Fatal("the new run's worktree was unregistered")
			}
			if data, _ := os.ReadFile(filepath.Join(config.GetAgentHomePath(scionDir, "dev"), "agent-info.json")); strings.Contains(string(data), "deleted") {
				t.Errorf("the new run's agent-info.json was soft-delete marked: %s", data)
			}

			// The new run's own delete removes everything.
			entries = nil
			rec = doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-new&deleteFiles=true&removeBranch=true")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("current delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
				t.Errorf("the current run's delete left the agent dir (stat err %v)", err)
			}
			// Pinned: the host behaviour (SCION_HOST_UID unset, cleared
			// above). The delete prunes the worktree record and, with
			// removeBranch, deletes the branch. Inside an agent container
			// util skips the prune, so both would stay.
			if branchExists(t, repo, "dev") {
				t.Error("the current run's delete with removeBranch kept the branch")
			}
			if worktreeRegistered(t, repo, worktree) {
				t.Error("the current run's delete left the worktree registered")
			}
		})
	}
}

// --- Create-failure cleanup ---

// A failed synchronous create of run A removes the agent files only while
// they are still A's: files recorded as run B's (a same-name recreate that
// took them over) survive; A's own files and legacy files are removed.
func TestSyncCreateFailure_RemovesOnlyItsOwnRunsFiles(t *testing.T) {
	for _, tc := range []struct {
		name, diskRun string
		wantKept      bool
	}{
		{"files recorded as another run's", "run-b", true},
		{"files recorded as this run's", "run-a", false},
		{"legacy files", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)
			mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
				writeAgentFiles(t, agentDir, "provisioned")
				recordRun(t, projectPath, "same-name", tc.diskRun)
				return nil, errors.New("container start failed")
			}
			body := fmt.Sprintf(`{"id":"agent-a-id","name":"same-name","projectPath":%q,"runId":"run-a","config":{"task":"t"}}`, projectPath)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code < 400 {
				t.Fatalf("expected the create to fail, got %d: %s", w.Code, w.Body.String())
			}
			_, err := os.Stat(agentDir)
			if tc.wantKept && err != nil {
				t.Errorf("run A's failure cleanup removed run B's files: %v", err)
			}
			if !tc.wantKept && !os.IsNotExist(err) {
				t.Errorf("run A's failure cleanup left its own files (stat err %v)", err)
			}
		})
	}
}

// The async launch's failure cleanup applies the same run check.
func TestCleanupAbortedLaunch_KeepsFilesOfOtherRun(t *testing.T) {
	for _, tc := range []struct {
		name, diskRun string
		wantKept      bool
	}{
		{"files recorded as another run's", "run-b", true},
		{"files recorded as this run's", "run-a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			agentDir := filepath.Join(projectDir, "agents", "agent-x")
			if err := os.MkdirAll(agentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			recordRun(t, projectDir, "agent-x", tc.diskRun)
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

			_, err := os.Stat(agentDir)
			if tc.wantKept && err != nil {
				t.Errorf("launch run-a's cleanup removed run-b's files: %v", err)
			}
			if !tc.wantKept && !os.IsNotExist(err) {
				t.Errorf("launch run-a's cleanup left its own files (stat err %v)", err)
			}
		})
	}
}

// An empty project path resolves the way DeleteAgentFiles resolves it (the
// broker's working project), so the run check is not skipped for it.
func TestAgentFilesRunOwner_EmptyProjectPathResolvesLikeDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	recordRun(t, scionDir, "dev", "run-new")
	t.Chdir(root)
	if got := agentFilesRunOwner("dev", "", "run-old"); got != "run-new" {
		t.Errorf("owner = %q, want run-new from the working project", got)
	}
	if got := agentFilesRunOwner("dev", "", "run-new"); got != "" {
		t.Errorf("owner for the recorded run = %q, want none", got)
	}
	if got := agentFilesRunOwner("dev", "", ""); got != "" {
		t.Errorf("owner without a run = %q, want none", got)
	}
}
