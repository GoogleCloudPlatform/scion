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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestCanonicalWorkspacePath_AliasesAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "project"), 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(real, alias))

	want, err := CanonicalWorkspacePath(filepath.Join(real, "project"))
	require.NoError(t, err)
	for _, p := range []string{
		filepath.Join(alias, "project"),
		filepath.Join(alias, "project") + string(filepath.Separator),
		filepath.Join(real, "x", "..", "project"),
	} {
		got, err := CanonicalWorkspacePath(p)
		require.NoError(t, err)
		assert.Equal(t, want, got, p)
	}

	// A path that does not exist yet has the key it will have once created,
	// whichever alias names it.
	missingViaAlias, err := CanonicalWorkspacePath(filepath.Join(alias, "project", "worktrees", "agent-1"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(real, "project", "worktrees", "agent-1"), 0o755))
	created, err := CanonicalWorkspacePath(filepath.Join(real, "project", "worktrees", "agent-1"))
	require.NoError(t, err)
	assert.Equal(t, created, missingViaAlias)

	// Relative paths resolve against the working directory.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	require.NoError(t, os.Chdir(alias))
	rel, err := CanonicalWorkspacePath("project")
	require.NoError(t, err)
	assert.Equal(t, want, rel)

	_, err = CanonicalWorkspacePath("")
	assert.Error(t, err)
}

// lockedWithin reports whether Lock(paths) succeeds within d.
func lockedWithin(l *WorkspaceLocks, d time.Duration, paths ...string) (func(), bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	release, err := l.Lock(ctx, paths...)
	return release, err == nil
}

func TestWorkspaceLocks_SamePathAliasAncestorAndDescendantExclude(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(project, alias))
	l := NewWorkspaceLocks()

	release, err := l.Lock(context.Background(), project)
	require.NoError(t, err)
	for _, p := range []string{project, alias, filepath.Join(alias, "worktrees", "a"), root} {
		if r, ok := lockedWithin(l, 20*time.Millisecond, p); ok {
			r()
			t.Fatalf("%s acquired while %s is held", p, project)
		}
	}
	// A sibling is independent.
	r, ok := lockedWithin(l, time.Second, filepath.Join(root, "other-project"))
	require.True(t, ok, "an unrelated path is not blocked")
	r()

	release()
	release() // idempotent
	r, ok = lockedWithin(l, time.Second, filepath.Join(alias, "worktrees", "a"))
	require.True(t, ok, "released")
	r()
}

func TestWorkspaceLocks_WaiterProceedsAfterRelease(t *testing.T) {
	l := NewWorkspaceLocks()
	p := filepath.Join(t.TempDir(), "project")
	release, err := l.Lock(context.Background(), p)
	require.NoError(t, err)
	got := make(chan struct{})
	go func() {
		r, err := l.Lock(context.Background(), filepath.Join(p, "worktrees"))
		if err == nil {
			r()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("the waiter acquired a path inside a held one")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter did not proceed after the release")
	}
}

func TestWorkspaceLocks_ContextEndsWaitHoldingNothing(t *testing.T) {
	l := NewWorkspaceLocks()
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	release, err := l.Lock(context.Background(), a)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = l.Lock(ctx, b, a)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "err = %v", err)
	// b was never taken by the cancelled call.
	r, ok := lockedWithin(l, time.Second, b)
	require.True(t, ok)
	r()
	release()
}

// waitQueued waits until n requests are waiting on l.
func waitQueued(t *testing.T, l *WorkspaceLocks, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		got := len(l.waiters)
		l.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests waiting, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWorkspaceLocks_BroadWaiterIsNotPassedByNarrowerOnes: while a narrow
// path inside a project is held and a project-wide request waits, a new
// request for another narrow path inside the project waits behind the
// project-wide one instead of passing it; it runs after the project-wide
// holder releases.
func TestWorkspaceLocks_BroadWaiterIsNotPassedByNarrowerOnes(t *testing.T) {
	l := NewWorkspaceLocks()
	project := filepath.Join(t.TempDir(), "project")
	a, b := filepath.Join(project, "a"), filepath.Join(project, "b")
	releaseA, err := l.Lock(context.Background(), a)
	require.NoError(t, err)

	broadHeld := make(chan func(), 1)
	go func() {
		r, err := l.Lock(context.Background(), project)
		if err == nil {
			broadHeld <- r
		}
	}()
	waitQueued(t, l, 1)

	if r, ok := lockedWithin(l, 30*time.Millisecond, b); ok {
		r()
		t.Fatal("a narrower request passed the waiting project-wide request")
	}
	releaseA()
	var releaseBroad func()
	select {
	case releaseBroad = <-broadHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("the project-wide request did not get the lock")
	}
	if r, ok := lockedWithin(l, 30*time.Millisecond, b); ok {
		r()
		t.Fatal("a narrower request ran while the project-wide one holds the lock")
	}
	releaseBroad()
	r, ok := lockedWithin(l, time.Second, b)
	require.True(t, ok)
	r()
}

// TestWorkspaceLocks_CancelledWaiterLeavesTheQueue: a waiting request whose
// context ends leaves the queue, and a request queued behind it that no
// held key blocks then proceeds.
func TestWorkspaceLocks_CancelledWaiterLeavesTheQueue(t *testing.T) {
	l := NewWorkspaceLocks()
	project := filepath.Join(t.TempDir(), "project")
	a, b := filepath.Join(project, "a"), filepath.Join(project, "b")
	releaseA, err := l.Lock(context.Background(), a)
	require.NoError(t, err)
	defer releaseA()

	ctx, cancel := context.WithCancel(context.Background())
	broadDone := make(chan error, 1)
	go func() {
		_, err := l.Lock(ctx, project)
		broadDone <- err
	}()
	waitQueued(t, l, 1)
	narrowHeld := make(chan func(), 1)
	go func() {
		r, err := l.Lock(context.Background(), b)
		if err == nil {
			narrowHeld <- r
		}
	}()
	waitQueued(t, l, 2)
	cancel()
	require.ErrorIs(t, <-broadDone, context.Canceled)
	select {
	case r := <-narrowHeld:
		r()
	case <-time.After(5 * time.Second):
		t.Fatal("the request behind a cancelled waiter never got the lock")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	assert.Empty(t, l.waiters)
}

// TestWorkspaceLocks_WithWorkspaceLockReleasesOnPanic: a panic while the
// lock is held releases it, so the path is not locked for the life of the
// process.
func TestWorkspaceLocks_WithWorkspaceLockReleasesOnPanic(t *testing.T) {
	s := &Server{}
	project := filepath.Join(t.TempDir(), "project")
	func() {
		defer func() { _ = recover() }()
		_ = s.withWorkspaceLock(context.Background(), func() { panic("boom") }, project)
	}()
	r, ok := lockedWithin(s.locks(), time.Second, project)
	require.True(t, ok, "the lock is still held after the panic")
	r()
}

// TestWorkspaceLocks_MultiPathNoDeadlockAndMutualExclusion: operations
// taking overlapping path sets in opposite orders never deadlock, and no two
// holders of overlapping paths run at once.
func TestWorkspaceLocks_MultiPathNoDeadlockAndMutualExclusion(t *testing.T) {
	l := NewWorkspaceLocks()
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	sets := [][]string{{a, b}, {b, a}, {a}, {b}, {filepath.Join(a, "x")}, {root}}
	var inA, inB atomic.Int32
	var wg sync.WaitGroup
	done := make(chan struct{})
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				set := sets[(g+i)%len(sets)]
				release, err := l.Lock(context.Background(), set...)
				if err != nil {
					t.Error(err)
					return
				}
				touchesA, touchesB := false, false
				for _, p := range set {
					touchesA = touchesA || overlaps(p, a)
					touchesB = touchesB || overlaps(p, b)
				}
				if touchesA && inA.Add(1) != 1 {
					t.Error("two holders of a at once")
				}
				if touchesB && inB.Add(1) != 1 {
					t.Error("two holders of b at once")
				}
				if touchesA {
					inA.Add(-1)
				}
				if touchesB {
					inB.Add(-1)
				}
				release()
			}
		}(g)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: lock holders did not finish")
	}
}

// TestWorkspaceLocks_SharedAcrossServers: two Runtime Broker servers given
// one service exclude each other on a project (also through an alias of
// its path); a server without one gets its own, so a single Runtime Broker
// is unaffected by another process-level service.
func TestWorkspaceLocks_SharedAcrossServers(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(project, alias))

	shared := NewWorkspaceLocks()
	newServer := func(locks *WorkspaceLocks) *Server {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		cfg.WorkspaceLocks = locks
		return New(cfg, &mockManager{}, nil)
	}
	a, b, solo := newServer(shared), newServer(shared), newServer(nil)
	require.Same(t, shared, a.workspaceLocks)
	require.NotNil(t, solo.workspaceLocks)
	require.NotSame(t, shared, solo.workspaceLocks)

	unlock, err := a.lockProjectWorkspace(context.Background(), project, filepath.Join(project, ".scion"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = b.lockProjectWorkspace(ctx, filepath.Join(alias), "")
	require.ErrorIs(t, err, context.DeadlineExceeded, "the other instance provisions the same project concurrently")

	u, err := solo.lockProjectWorkspace(context.Background(), project, "")
	require.NoError(t, err, "a server with its own service is not coordinated with the host's")
	u()
	unlock()
	u, err = b.lockProjectWorkspace(context.Background(), alias, "")
	require.NoError(t, err)
	u()
}

// hubProjectDir creates a hub-managed project directory for slug under the
// global dir and returns it.
func hubProjectDir(t *testing.T, slug, projectID string) string {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	dir := filepath.Join(globalDir, "projects", slug)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".scion", "project-id"), []byte(projectID+"\n"), 0o644))
	return dir
}

// TestWorkspaceLocks_ProjectDeleteKeepsWorkspaceAnotherInstanceUses: a
// project removal on instance A is refused while instance B of the same
// host still has live agents in the project, and proceeds once B's agents
// are deleted. A singleton's own agents do not change the legacy behaviour.
func TestWorkspaceLocks_ProjectDeleteKeepsWorkspaceAnotherInstanceUses(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	dir := hubProjectDir(t, "shared-proj", "proj-1")
	b.own(t, d, "proj-1", "agent-b1", "worker", "cid-b1")

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id=proj-1", "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.DirExists(t, dir, "the workspace another instance uses is kept")
	// Without project_id the project's own ID is read from its directory.
	w = serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj", "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// B's own agents never block B (the singleton behaviour is unchanged).
	require.NoError(t, b.srv.ownership.SetRecordState("proj-1", "agent-b1", OwnershipStateDeleting))
	w = serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id=proj-1", "")
	require.Equal(t, http.StatusConflict, w.Code, "a deleting agent still uses the workspace")
	require.NoError(t, b.srv.ownership.SetRecordState("proj-1", "agent-b1", OwnershipStateDeleted))
	w = serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id=proj-1", "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoDirExists(t, dir)
}

// TestWorkspaceLocks_ProjectDeleteWaitsForProvisioning: a project removal
// waits while another instance provisions in the project (through an alias
// of its path) and runs after that finishes.
func TestWorkspaceLocks_ProjectDeleteWaitsForProvisioning(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	dir := hubProjectDir(t, "busy-proj", "proj-2")
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(dir, alias))

	unlock, err := b.srv.lockProjectWorkspace(context.Background(), filepath.Join(alias, "workspace"), alias)
	require.NoError(t, err)
	done := make(chan int, 1)
	go func() {
		done <- serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/busy-proj?project_id=proj-2", "").Code
	}()
	select {
	case code := <-done:
		t.Fatalf("the project was removed (%d) during another instance's provisioning", code)
	case <-time.After(50 * time.Millisecond):
	}
	require.DirExists(t, dir)
	unlock()
	select {
	case code := <-done:
		require.Equal(t, http.StatusNoContent, code)
	case <-time.After(10 * time.Second):
		t.Fatal("the project removal did not proceed after provisioning finished")
	}
	require.NoDirExists(t, dir)
}

// TestWorkspaceLocks_AgentFileLockPaths: an agent's file operations take
// the project root (excluding provisioning in the same project, also
// through an alias) and the global agents entry; for the global project only
// the agent's own entries, never the global directory.
func TestWorkspaceLocks_AgentFileLockPaths(t *testing.T) {
	setupTestScionEnv(t)
	dir := hubProjectDir(t, "files-proj", "proj-3")
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	paths := agentFileLockPaths(filepath.Join(dir, ".scion"), "worker")
	assert.Contains(t, paths, dir)
	globalAgents, err := config.GetGlobalAgentsDir()
	require.NoError(t, err)
	for _, p := range paths {
		assert.False(t, p == globalAgents || strings.HasPrefix(p, globalAgents+string(filepath.Separator)),
			"another project's lock set includes the global project's agent entry %s", p)
	}

	s := New(DefaultServerConfig(), &mockManager{}, nil)
	unlock, err := s.lockProjectWorkspace(context.Background(), filepath.Join(dir, "workspace"), dir)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = s.lockAgentFiles(ctx, filepath.Join(dir, ".scion"), "worker")
	require.ErrorIs(t, err, context.DeadlineExceeded, "agent file cleanup waits for provisioning in its project")
	unlock()

	globalPaths := agentFileLockPaths(globalDir, "worker")
	for _, p := range globalPaths {
		assert.NotEqual(t, globalDir, p, "the global directory itself is never locked")
		assert.NotEqual(t, filepath.Dir(globalDir), p)
	}
	assert.Contains(t, globalPaths, filepath.Join(globalDir, "agents", "worker"))
	assert.Contains(t, globalPaths, filepath.Join(globalDir, "workspace", "worker"))
}

// TestWorkspaceLocks_AgentDeleteWaitsForProvisioning: an agent delete that
// removes files waits while another instance provisions in the agent's
// project; the runtime delete and file removal run after that.
func TestWorkspaceLocks_AgentDeleteWaitsForProvisioning(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	dir := hubProjectDir(t, "del-proj", "proj-4")
	a.own(t, d, "proj-4", "agent-a4", "worker", "cid-a4")
	d.mu.Lock()
	for i := range d.objects {
		if d.objects[i].ContainerID == "cid-a4" {
			d.objects[i].ProjectPath = filepath.Join(dir, ".scion")
			d.objects[i].Labels["scion.project_path"] = filepath.Join(dir, ".scion")
		}
	}
	d.mu.Unlock()

	unlock, err := b.srv.lockProjectWorkspace(context.Background(), filepath.Join(dir, "workspace"), dir)
	require.NoError(t, err)
	done := make(chan int, 1)
	go func() {
		done <- serveFlat(a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-4&deleteFiles=true", "").Code
	}()
	select {
	case code := <-done:
		t.Fatalf("the agent delete ran (%d) during another instance's provisioning", code)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, d.recorded(), "nothing deleted while the project is being provisioned")
	unlock()
	select {
	case code := <-done:
		require.Less(t, code, 300)
	case <-time.After(10 * time.Second):
		t.Fatal("the agent delete did not proceed after provisioning finished")
	}
	assert.Contains(t, d.recorded(), "delete:cid-a4")
}

// TestWorkspaceLocks_WorkspaceDownloadWaitsForProjectLock: materializing a
// hub project's workspace waits while another instance holds the project
// (a removal or provisioning) and runs after it is released.
func TestWorkspaceLocks_WorkspaceDownloadWaitsForProjectLock(t *testing.T) {
	setupTestScionEnv(t)
	locks := NewWorkspaceLocks()
	newSrv := func() *Server {
		cfg := DefaultServerConfig()
		cfg.StateDir = t.TempDir()
		cfg.WorkspaceLocks = locks
		return New(cfg, &mockManager{}, nil)
	}
	a, b := newSrv(), newSrv()
	var downloads atomic.Int32
	a.SetWorkspaceDownloader(func(context.Context, string, string, string) error {
		downloads.Add(1)
		return nil
	})
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	projectDir := filepath.Join(globalDir, "projects", "dl-proj")

	unlock, err := b.lockProjectWorkspace(context.Background(), filepath.Join(projectDir, "workspace"), projectDir)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := a.downloadWorkspaceFromGCS(context.Background(), CreateAgentRequest{
			ID: "agent-dl", Name: "agent-dl", ProjectID: "proj-dl", ProjectSlug: "dl-proj",
			WorkspaceStoragePath: "uploads/dl", WorkspaceStorageBucket: "bucket"}, api.StartOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the workspace was materialized (%v) while another instance holds the project", err)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Zero(t, downloads.Load())
	unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not proceed after the release")
	}
	assert.Equal(t, int32(1), downloads.Load())
}

// TestWorkspaceLocks_WorkspaceApplyWaitsForTheWorkspace: a workspace apply
// writes into the agent's workspace only while no other instance holds it.
func TestWorkspaceLocks_WorkspaceApplyWaitsForTheWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, "myproject")
	worktreePath := filepath.Join(tmpDir, ".scion_worktrees", filepath.Base(tmpDir), "test-agent")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	locks := NewWorkspaceLocks()
	cfg := DefaultServerConfig()
	cfg.StorageBucket = "test-bucket"
	cfg.WorkspaceLocks = locks
	mgr := &mockAgentManager{agents: []api.AgentInfo{{Name: "test-agent", ContainerID: "test-agent", ProjectPath: projectPath}}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" },
		GetWorkspacePathFunc: func(context.Context, string) (string, error) { return "", nil }}
	srv := New(cfg, mgr, rt)
	var downloads atomic.Int32
	srv.SetWorkspaceDownloader(func(context.Context, string, string, string) error { downloads.Add(1); return nil })

	other := &Server{workspaceLocks: locks}
	unlock, err := other.locks().Lock(context.Background(), worktreePath)
	require.NoError(t, err)
	done := make(chan int, 1)
	go func() {
		body, _ := json.Marshal(WorkspaceApplyRequest{Slug: "test-agent", StoragePath: "workspaces/project/agent"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/apply", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleWorkspaceApply(rec, req)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		t.Fatalf("apply ran (%d) while another instance holds the workspace", code)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Zero(t, downloads.Load())
	unlock()
	select {
	case code := <-done:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(10 * time.Second):
		t.Fatal("apply did not proceed after the release")
	}
	assert.Equal(t, int32(1), downloads.Load())
}
