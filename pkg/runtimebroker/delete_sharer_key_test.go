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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#3405: a Runtime Broker provisions a hub agent's
// worktree and sharer registry entry under its hub agent ID, so a delete
// must find them under that ID (the hub's agentId), not the agent's name.

// sharerFilesManager is a filteringMockManager whose DeleteTarget also
// removes the agent's files the way AgentManager does, through
// agent.DeleteHubAgentFiles with the sharer key the handler passes and the
// Hub project ID from ctx.
type sharerFilesManager struct {
	filteringMockManager
}

func (m *sharerFilesManager) DeleteTarget(ctx context.Context, agentName, sharerKey string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if _, err := m.filteringMockManager.DeleteTarget(ctx, agentName, sharerKey, ref, deleteFiles, projectPath, removeBranch); err != nil {
		return false, err
	}
	if !deleteFiles {
		return false, nil
	}
	deleted, err := agent.DeleteHubAgentFiles(agentName, sharerKey, projectPath, api.HubProjectIDFromContext(ctx), removeBranch)
	if err != nil && !errors.Is(err, agent.ErrAgentProjectUnresolved) {
		return deleted, fmt.Errorf("%w: %w", agent.ErrAgentFilesDelete, err)
	}
	return deleted, nil
}

// sharerFixture is a hub-native worktree-per-agent project on a broker:
// its shared base at <project>/workspace, cloned from a real repository.
type sharerFixture struct {
	srv      *Server
	mgr      *sharerFilesManager
	scionDir string
	base     string
	gc       *api.GitCloneConfig
}

func newSharerFixture(t *testing.T) *sharerFixture {
	t.Helper()
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	mgr := &sharerFilesManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	scionDir, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev-a")
	makeHubProject(t, home, "proj-b", scopeProjB, "dev-b")
	return &sharerFixture{
		srv:      srv,
		mgr:      mgr,
		scionDir: scionDir,
		base:     filepath.Join(filepath.Dir(scionDir), "workspace"),
		gc:       &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"},
	}
}

// provision provisions an agent registered under key on branch, as a
// broker start does (ensureWorktree), and returns its worktree path.
func (f *sharerFixture) provision(t *testing.T, key, branch string) string {
	t.Helper()
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: filepath.Dir(f.scionDir), ProjectID: scopeProjB, AgentID: key,
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.HostPath != f.base {
		t.Fatalf("shared base %q, want %q", resolved.HostPath, f.base)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: scopeProjB, AgentID: key, AgentName: branch, GitClone: f.gc,
	}); err != nil {
		t.Fatalf("provision %s: %v", key, err)
	}
	return provision.WorktreePath(f.base, key)
}

func (f *sharerFixture) marker(branch string) string {
	return filepath.Join(f.base, ".git", "scion-sharers", branch+".json")
}

func (f *sharerFixture) sharers(t *testing.T, branch string) []string {
	t.Helper()
	got, _, err := provision.ListSharers(f.base, "", branch)
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	return got
}

// assertTornDown checks that nothing of the worktree at wt (registered by
// key on branch) is left under the shared base.
func (f *sharerFixture) assertTornDown(t *testing.T, key, wt, branch string) {
	t.Helper()
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still present: %v", wt, err)
	}
	if _, err := os.Lstat(filepath.Join(f.base, ".git", "worktrees", key)); !os.IsNotExist(err) {
		t.Errorf("git worktree entry for %s still present: %v", key, err)
	}
	if _, err := os.Lstat(f.marker(branch)); !os.IsNotExist(err) {
		t.Errorf("sharer registry file for %s still present: %v", branch, err)
	}
	for _, dir := range []string{filepath.Join(f.base, "worktrees"), filepath.Join(f.base, ".git", "worktrees"), filepath.Join(f.base, ".git", "scion-sharers")} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			t.Errorf("%s is not empty after the last delete: %v", dir, entries)
		}
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func withAgentIDLabel(e api.AgentInfo, agentID string) api.AgentInfo {
	labels := make(map[string]string, len(e.Labels)+1)
	for k, v := range e.Labels {
		labels[k] = v
	}
	labels[api.LabelAgentID] = agentID
	e.Labels = labels
	return e
}

const sharerDeleteFlags = "&deleteFiles=true&removeBranch=true"

// A creates the worktree and B joins it. Deleting B unregisters B and
// keeps the worktree; deleting A, the last sharer, removes the worktree,
// git's entry for it and the registry file.
func TestDeleteAgent_SharedWorktree_HubAgentID(t *testing.T) {
	f := newSharerFixture(t)
	wtA := f.provision(t, "uuid-a", "shared")
	if wtB := f.provision(t, "uuid-b", "shared"); wtB == wtA {
		t.Fatal("setup: distinct keys share one worktree path")
	}
	if got := f.sharers(t, "shared"); !slices.Equal(got, []string{"uuid-a", "uuid-b"}) {
		t.Fatalf("setup: sharers = %v", got)
	}
	f.mgr.agents = []api.AgentInfo{
		withAgentIDLabel(labelled("dev-a", "cid-a", scopeProjB, f.scionDir), "uuid-a"),
		withAgentIDLabel(labelled("dev-b", "cid-b", scopeProjB, f.scionDir), "uuid-b"),
	}

	rec := doDelete(t, f.srv, "dev-b", "projectId="+scopeProjB+"&agentId=uuid-b"+sharerDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete B: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := f.mgr.LastDeleteSharerKey(); got != "uuid-b" {
		t.Errorf("delete B: sharer key %q, want uuid-b", got)
	}
	if got := f.sharers(t, "shared"); !slices.Equal(got, []string{"uuid-a"}) {
		t.Errorf("after deleting B: sharers = %v, want [uuid-a]", got)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Errorf("after deleting B: the shared worktree is gone: %v", err)
	}

	f.mgr.agents = f.mgr.agents[:1]
	rec = doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete A: got %d: %s", rec.Code, rec.Body.String())
	}
	f.assertTornDown(t, "uuid-a", wtA, "shared")
}

// An agentId other than the runtime entry's agent_id label is refused:
// nothing is deleted.
func TestDeleteAgent_AgentIDLabelMismatch_Refused(t *testing.T) {
	f := newSharerFixture(t)
	wtA := f.provision(t, "uuid-a", "shared")
	before := readFileBytes(t, f.marker("shared"))
	f.mgr.agents = []api.AgentInfo{withAgentIDLabel(labelled("dev-a", "cid-a", scopeProjB, f.scionDir), "uuid-x")}

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerDeleteFlags)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "belongs to another agent") {
		t.Errorf("409 body does not name the agent mismatch: %s", rec.Body.String())
	}
	if n := f.mgr.DeleteCalls(); n != 0 {
		t.Errorf("DeleteTarget called %d times, want 0", n)
	}
	if after := readFileBytes(t, f.marker("shared")); !bytes.Equal(before, after) {
		t.Errorf("registry file changed: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Errorf("worktree removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.scionDir, "agents", "dev-a")); err != nil {
		t.Errorf("agent directory removed: %v", err)
	}
}

// An agent labelled with its own name (started outside hub dispatch and
// provisioned by name) is not a mismatch: it is deleted, and its worktree
// is found under its name.
func TestDeleteAgent_AgentIDLabelIsName_NotRefused(t *testing.T) {
	f := newSharerFixture(t)
	wt := f.provision(t, "dev-a", "shared")
	f.mgr.agents = []api.AgentInfo{withAgentIDLabel(labelled("dev-a", "cid-a", scopeProjB, f.scionDir), "dev-a")}

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got := f.mgr.LastDeleteContainerID(); got != "cid-a" {
		t.Errorf("deleted container %q, want cid-a", got)
	}
	f.assertTornDown(t, "dev-a", wt, "shared")
}

// With no container (already removed, or never started), the delete uses
// the hub's agent ID.
func TestDeleteAgent_NoContainer_HubAgentIDTearsDown(t *testing.T) {
	f := newSharerFixture(t)
	wt := f.provision(t, "uuid-a", "shared")

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a"+sharerDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got := f.mgr.LastDeleteSharerKey(); got != "uuid-a" {
		t.Errorf("sharer key %q, want uuid-a", got)
	}
	f.assertTornDown(t, "uuid-a", wt, "shared")
}

// An older hub sends no agentId: the runtime entry's agent_id label is the
// key.
func TestDeleteAgent_NoHubAgentID_UsesLabel(t *testing.T) {
	f := newSharerFixture(t)
	wt := f.provision(t, "uuid-a", "shared")
	f.mgr.agents = []api.AgentInfo{withAgentIDLabel(labelled("dev-a", "cid-a", scopeProjB, f.scionDir), "uuid-a")}

	rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+sharerDeleteFlags)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	f.assertTornDown(t, "uuid-a", wt, "shared")
}

// An agentId that is not a single clean path element is refused before
// anything is resolved or removed.
func TestDeleteAgent_InvalidAgentID_Refused(t *testing.T) {
	f := newSharerFixture(t)
	wt := f.provision(t, "uuid-a", "shared")
	before := readFileBytes(t, f.marker("shared"))
	f.mgr.agents = []api.AgentInfo{withAgentIDLabel(labelled("dev-a", "cid-a", scopeProjB, f.scionDir), "uuid-a")}

	for _, bad := range []string{"..", "..%2Fx", "a%2Fb", "a%5Cb"} {
		t.Run(bad, func(t *testing.T) {
			rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId="+bad+sharerDeleteFlags)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if n := f.mgr.DeleteCalls(); n != 0 {
				t.Errorf("DeleteTarget called %d times, want 0", n)
			}
		})
	}
	if after := readFileBytes(t, f.marker("shared")); !bytes.Equal(before, after) {
		t.Errorf("registry file changed: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("worktree removed: %v", err)
	}
}

// Files that belong to another run, or whose owner is unknown
// (ptone/scion#4771), are kept, and so are the agent's sharer registry
// entry and worktree: the requested run's container is deleted and nothing
// else.
func TestDeleteAgent_FilesNotThisRuns_KeepsSharerAndWorktree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, scionDir string)
	}{
		{"another run's files", func(t *testing.T, scionDir string) { recordRun(t, scionDir, "dev-a", "run-new") }},
		{"owner unknown", func(t *testing.T, scionDir string) { plantHostState(t, scionDir, "dev-a", `{"runId":`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharerFixture(t)
			wt := f.provision(t, "uuid-a", "shared")
			tc.plant(t, f.scionDir)
			before := readFileBytes(t, f.marker("shared"))
			f.mgr.agents = []api.AgentInfo{withRun(withAgentIDLabel(labelled("dev-a", "cid-old", scopeProjB, f.scionDir), "uuid-a"), "run-old")}

			rec := doDelete(t, f.srv, "dev-a", "projectId="+scopeProjB+"&agentId=uuid-a&runId=run-old"+sharerDeleteFlags)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
			}
			if f.mgr.DeleteCalls() != 1 || f.mgr.LastDeleteContainerID() != "cid-old" {
				t.Fatalf("got %d deletes, last %q; want the requested run's cid-old", f.mgr.DeleteCalls(), f.mgr.LastDeleteContainerID())
			}
			if f.mgr.LastDeleteFiles() {
				t.Error("the delete removed files that are not the requested run's")
			}
			if after := readFileBytes(t, f.marker("shared")); !bytes.Equal(before, after) {
				t.Errorf("registry file changed: %s -> %s", before, after)
			}
			if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
				t.Errorf("worktree removed: %v", err)
			}
		})
	}
}

func TestDeleteSharerKey(t *testing.T) {
	for _, tc := range []struct {
		name, hub, label, agentName string
		wantKey                     string
		wantMismatch                bool
	}{
		{"hub ID, no label", "uuid-a", "", "dev", "uuid-a", false},
		{"hub ID equals label", "uuid-a", "uuid-a", "dev", "uuid-a", false},
		{"label is the name", "uuid-a", "dev", "dev", "", false},
		{"label is the slug", "uuid-a", "my-dev", "My Dev", "", false},
		{"no hub ID, label", "", "uuid-a", "dev", "uuid-a", false},
		{"no hub ID, label is the name", "", "dev", "dev", "", false},
		{"no hub ID, invalid label", "", "../x", "dev", "", false},
		{"no hub ID, no label", "", "", "dev", "", false},
		{"label is another ID", "uuid-a", "uuid-x", "dev", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, mismatch := deleteSharerKey(tc.hub, tc.label, tc.agentName)
			if key != tc.wantKey || mismatch != tc.wantMismatch {
				t.Errorf("deleteSharerKey(%q, %q, %q) = %q, %v; want %q, %v",
					tc.hub, tc.label, tc.agentName, key, mismatch, tc.wantKey, tc.wantMismatch)
			}
		})
	}
}

// A failed synchronous create removes the worktree and sharer registry
// entry it provisioned under the hub agent ID.
func TestSyncCreateFailure_RemovesWorktreeUnderHubAgentID(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)
	base := filepath.Join(filepath.Dir(projectPath), "workspace")
	writeAgentFiles(t, agentDir, "provisioned")
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a-id", AgentName: "same-name",
		GitClone: &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"},
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt := provision.WorktreePath(base, "agent-a-id")

	var started atomic.Bool
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		started.Store(true)
		return nil, errors.New("container start failed")
	}
	body := fmt.Sprintf(`{"id":"agent-a-id","name":"same-name","projectPath":%q,"config":{"task":"t"}}`, projectPath)
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
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("the failure cleanup left the worktree: %v", err)
	}
	if _, _, found, err := provision.FindBranchForAgent(base, "", "agent-a-id"); err != nil || found {
		t.Errorf("the failure cleanup left the sharer registry entry (found=%v, err=%v)", found, err)
	}
}

// An aborted async create launch removes the worktree and sharer registry
// entry it provisioned under the hub agent ID.
func TestCleanupLaunchFiles_RemovesWorktreeUnderHubAgentID(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentDir := filepath.Join(projectDir, "agents", "agent-x")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(filepath.Dir(projectDir), "workspace")
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-id-x", AgentName: "agent-x",
		GitClone: &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"},
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt := provision.WorktreePath(base, "agent-id-x")
	if err := writeLaunchMarker(projectDir, false, "agent-x", "L1"); err != nil {
		t.Fatal(err)
	}
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id-x", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})

	if err := srv.cleanupLaunchFiles(rec, api.StartOptions{Name: "agent-x", ProjectPath: projectDir, RunID: "run-a"}, false, "agent-x"); err != nil {
		t.Fatalf("cleanupLaunchFiles: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("the launch cleanup left the worktree: %v", err)
	}
	if _, _, found, err := provision.FindBranchForAgent(base, "", "agent-id-x"); err != nil || found {
		t.Errorf("the launch cleanup left the sharer registry entry (found=%v, err=%v)", found, err)
	}
}

func TestStartSharerKey(t *testing.T) {
	for in, want := range map[string]string{"uuid-a": "uuid-a", "": "", "..": "", "a/b": ""} {
		if got := startSharerKey(in); got != want {
			t.Errorf("startSharerKey(%q) = %q, want %q", in, got, want)
		}
	}
}
