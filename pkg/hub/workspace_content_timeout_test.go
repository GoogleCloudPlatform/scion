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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangReadDirFor makes workspaceReadDir block for any path under hungPrefix
// until the test ends, and shortens workspaceContentTimeout. Other paths use
// os.ReadDir. These tests mutate package-level seams and must not call
// t.Parallel(); parallel tests in this package run only after the serial
// ones, so they cannot observe the swapped values.
func hangReadDirFor(t *testing.T, hungPrefix string) {
	t.Helper()
	release := make(chan struct{})
	prevRead, prevTimeout := workspaceReadDir, workspaceContentTimeout
	workspaceReadDir = func(dir string) ([]os.DirEntry, error) {
		if strings.HasPrefix(dir, hungPrefix) {
			<-release
			return nil, os.ErrDeadlineExceeded
		}
		return os.ReadDir(dir)
	}
	workspaceContentTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		workspaceReadDir, workspaceContentTimeout = prevRead, prevTimeout
	})
}

func TestProbeWorkspaceContent(t *testing.T) {
	dir := t.TempDir()

	has, err := probeWorkspaceContent(filepath.Join(dir, "missing"))
	require.NoError(t, err)
	assert.False(t, has, "missing dir has no content")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".scion"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "shared-dirs"), 0755))
	has, err = probeWorkspaceContent(dir)
	require.NoError(t, err)
	assert.False(t, has, "infrastructure dirs alone are not content")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644))
	has, err = probeWorkspaceContent(dir)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestProbeWorkspaceContent_TimesOutOnHungRead(t *testing.T) {
	dir := t.TempDir()
	hangReadDirFor(t, dir)

	start := time.Now()
	has, err := probeWorkspaceContent(dir)
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.False(t, has)
	assert.Less(t, elapsed, 5*time.Second, "probe must not block on a hung read")
}

// hungPathFixture is a temp HOME with a legacy local project dir that has
// content.
type hungPathFixture struct {
	tmpHome  string
	slug     string
	localDir string
}

func newHungPathFixture(t *testing.T, slug string) hungPathFixture {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	localDir := filepath.Join(tmpHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))
	return hungPathFixture{tmpHome: tmpHome, slug: slug, localDir: localDir}
}

func nfsConfig(mountRoot string) *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
}

// A hung NFS mount must not resolve to either path: the legacy local path
// might be wrong (the project may live on NFS), and the empty-looking NFS
// path might be wrong (a legacy project lives locally). It returns an error
// wrapping errWorkspaceContentTimeout, and logs on the projects logger.
func TestServerHubManagedProjectPath_NFSHungMountReturnsError(t *testing.T) {
	f := newHungPathFixture(t, "hung-project")
	mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)

	logs := captureSlog(t) // before testServer: the projects logger snapshots slog.Default()
	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

	path, err := srv.hubManagedProjectPath(f.slug)
	require.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Empty(t, path)
	nfsPath := filepath.Join(mountRoot, "share1", "hub-projects", f.slug)
	assert.Contains(t, logs.String(), "Workspace storage did not respond")
	assert.Contains(t, logs.String(), "path="+nfsPath)
}

// N2: durable NFS path empty, legacy local path hangs. The project might
// live locally, so this is an error too, not the durable path.
func TestServerHubManagedProjectPath_NFSEmptyLocalHungReturnsError(t *testing.T) {
	f := newHungPathFixture(t, "local-hung-project")
	mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1", "hub-projects", f.slug), 0755))
	hangReadDirFor(t, f.localDir)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

	path, err := srv.hubManagedProjectPath(f.slug)
	require.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Empty(t, path)
}

// NFS has content: the local path is never probed, so a hung local path
// does not matter.
func TestServerHubManagedProjectPath_NFSContentSkipsHungLocal(t *testing.T) {
	f := newHungPathFixture(t, "nfs-content-project")
	mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
	nfsDir := filepath.Join(mountRoot, "share1", "hub-projects", f.slug)
	require.NoError(t, os.MkdirAll(nfsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nfsDir, "nfs.txt"), []byte("nfs"), 0644))
	hangReadDirFor(t, f.localDir)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

	path, err := srv.hubManagedProjectPath(f.slug)
	require.NoError(t, err)
	assert.Equal(t, nfsDir, path)
}

func cloudRunVolumeConfig() *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "workspace-vol"},
	}
}

// Same guarantees for the volume-backed (Cloud Run / GKE) branch.
func TestServerHubManagedProjectPath_VolumeHungMountReturnsError(t *testing.T) {
	f := newHungPathFixture(t, "hung-vol-project")
	mountBase := filepath.Join(f.tmpHome, "mnt")
	setVolumeMountBase(t, mountBase)
	hangReadDirFor(t, mountBase)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = cloudRunVolumeConfig()

	path, err := srv.hubManagedProjectPath(f.slug)
	require.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Empty(t, path)
}

func TestServerHubManagedProjectPath_VolumeEmptyLocalHungReturnsError(t *testing.T) {
	f := newHungPathFixture(t, "local-hung-vol-project")
	mountBase := filepath.Join(f.tmpHome, "mnt")
	setVolumeMountBase(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol", "projects", "hub-projects", f.slug), 0755))
	hangReadDirFor(t, f.localDir)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = cloudRunVolumeConfig()

	path, err := srv.hubManagedProjectPath(f.slug)
	require.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Empty(t, path)
}

func TestWriteWorkspaceStorageUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	assert.False(t, writeWorkspaceStorageUnavailable(rec, fmt.Errorf("other")))
	assert.Equal(t, http.StatusOK, rec.Code, "nothing written for other errors")

	rec = httptest.NewRecorder()
	wrapped := fmt.Errorf("workspace content check for /mnt/x: %w", errWorkspaceContentTimeout)
	assert.True(t, writeWorkspaceStorageUnavailable(rec, wrapped))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotContains(t, rec.Body.String(), "/mnt/x", "response must not leak the path")
}

func TestProjectPathResolveError(t *testing.T) {
	err := projectPathResolveError(fmt.Errorf("check /mnt/x: %w", errWorkspaceContentTimeout), "failed to resolve project path")
	assert.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.NotContains(t, err.Error(), "/mnt/x")

	err = projectPathResolveError(fmt.Errorf("slug /etc: bad"), "failed to resolve project path")
	assert.NotErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Equal(t, "failed to resolve project path", err.Error())
}

// End to end: the project workspace files endpoint answers 503 (not 409 or
// 500, and not a hang) when the workspace storage mount does not respond.
func TestProjectWorkspaceList_HungStorageReturns503(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Hung Storage")

	mountRoot := filepath.Join(tmpHome, "nfs-mount")
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)
	hangReadDirFor(t, mountRoot)

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")
}

// When the project path cannot be resolved (here: a hung workspace mount),
// post-deletion filesystem cleanup is skipped. The skip must be logged so
// a left-behind directory is visible to operators.
func TestExecutePostDeletionEffects_LogsUnresolvedProjectPath(t *testing.T) {
	f := newHungPathFixture(t, "deleted-hung-project")
	mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)

	logs := captureSlog(t) // before testServer: the projects logger snapshots slog.Default()
	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

	project := &store.Project{ID: "proj-deleted-hung", Slug: f.slug}
	srv.executePostDeletionEffects(context.Background(), project.ID, project, deletionEffectInputs{})

	out := logs.String()
	assert.Contains(t, out, "skipping removal, the directory may be left behind")
	assert.Contains(t, out, "project_id="+project.ID)
	assert.Contains(t, out, "slug="+f.slug)
	assert.DirExists(t, f.localDir, "nothing is removed when the path is unresolved")
}

// N4: concurrent and repeated probes of the same directory share one read,
// so a hung mount costs at most one stuck goroutine (and OS thread) per
// directory. Once the read returns, the next probe starts a fresh read.
func TestProbeWorkspaceContent_DedupesInFlightReads(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0644))

	var reads atomic.Int32
	release := make(chan struct{})
	prevRead, prevTimeout := workspaceReadDir, workspaceContentTimeout
	workspaceReadDir = func(d string) ([]os.DirEntry, error) {
		reads.Add(1)
		<-release
		return os.ReadDir(d)
	}
	workspaceContentTimeout = 30 * time.Millisecond
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		workspaceReadDir, workspaceContentTimeout = prevRead, prevTimeout
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := probeWorkspaceContent(dir)
			assert.ErrorIs(t, err, errWorkspaceContentTimeout)
		}()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		_, err := probeWorkspaceContent(dir)
		assert.ErrorIs(t, err, errWorkspaceContentTimeout)
	}
	assert.Equal(t, int32(1), reads.Load(), "a hung directory must have only one read in flight")

	// Unblock the stuck read; it removes itself from the in-flight map.
	close(release)
	released = true
	require.Eventually(t, func() bool {
		_, inFlight := workspaceProbesInFlight.Load(dir)
		return !inFlight
	}, 5*time.Second, 5*time.Millisecond)

	has, err := probeWorkspaceContent(dir)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, int32(2), reads.Load(), "after the read returns, the next probe reads again")
}

// N3 (round 2): the slug-rename skip is logged, and nothing is renamed.
func TestMigrateProjectSlug_LogsUnresolvedProjectPath(t *testing.T) {
	f := newHungPathFixture(t, "old-slug-hung")
	mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)

	logs := captureSlog(t) // before testServer: the projects logger snapshots slog.Default()
	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

	project := &store.Project{ID: "proj-rename-hung", Slug: "new-slug-hung", Name: "Renamed"}
	srv.migrateProjectSlug(context.Background(), project, f.slug)

	out := logs.String()
	assert.Contains(t, out, "skipping rename, the directory may keep the old slug")
	assert.Contains(t, out, "project_id="+project.ID)
	assert.Contains(t, out, "slug="+f.slug)
	assert.Contains(t, out, "new_slug="+project.Slug)
	assert.DirExists(t, f.localDir, "nothing is renamed when the path is unresolved")
	assert.NoDirExists(t, filepath.Join(filepath.Dir(f.localDir), project.Slug))
}

// hungStorageServer returns a test server whose workspace storage is an NFS
// mount that never answers.
func hungStorageServer(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	mountRoot := filepath.Join(tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)
	srv, s := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)
	return srv, s, mountRoot
}

func requireNoProjectNamed(t *testing.T, s store.Store, name string) {
	t.Helper()
	res, err := s.ListProjects(context.Background(), store.ProjectFilter{Name: name}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, res.Items, "project %q must be rolled back", name)
}

// N2 (round 2): hub-native project create rolls back and answers 503 when
// workspace storage does not respond.
func TestCreateProject_HubNative_HungStorageReturns503(t *testing.T) {
	srv, s, mountRoot := hungStorageServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "Hung Native"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")
	requireNoProjectNamed(t, s, "Hung Native")
}

// N1 (round 2): shared-workspace project create answers a plain 503 (no
// clone wording, no path) and rolls back.
func TestCreateProject_SharedWorkspace_HungStorageReturns503(t *testing.T) {
	srv, s, mountRoot := hungStorageServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:          "Hung Shared",
		GitRemote:     "github.com/test/hung-shared",
		WorkspaceMode: "shared",
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")
	assert.NotContains(t, rec.Body.String(), "clone", "storage timeout is not a clone failure")
	requireNoProjectNamed(t, s, "Hung Shared")
}

// N2 (round 2): cloning into a hub-native project rolls back and answers
// 503 when workspace storage does not respond.
func TestProjectClone_HubNative_HungStorageReturns503(t *testing.T) {
	srv, s, mountRoot := hungStorageServer(t)
	src := &store.Project{ID: tid("project-hung-clone-src"), Name: "Hung Clone Source", Slug: "hung-clone-source"}
	require.NoError(t, s.CreateProject(context.Background(), src))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Hung Clone"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")
	requireNoProjectNamed(t, s, "Hung Clone")
}

// B1 (round 2): agent create in a hub-native project answers 503 when the
// workspace path cannot be resolved, before any agent row exists or any
// dispatch happens. Creating it anyway would run the agent against the
// broker's legacy local project path.
func TestCreateAgent_HungStorageReturns503NoAgentNoDispatch(t *testing.T) {
	srv, s, mountRoot := hungStorageServer(t)
	ctx := context.Background()
	disp := &mockDispatcher{}
	srv.SetDispatcher(disp)

	broker := &store.RuntimeBroker{
		ID: tid("broker-hung-storage"), Slug: "hung-storage-broker",
		Name: "Hung Storage Broker", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("project-hung-agent"), Slug: "hung-agent", Name: "Hung Agent Project"}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":            "hung-agent",
		"projectId":       project.ID,
		"runtimeBrokerId": broker.ID,
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")

	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "no agent row may be created")
	assert.Empty(t, disp.dispatchedAgents, "nothing may be dispatched")
}

// B1 (round 2): deriveAgentConfig propagates the timeout (used by create,
// scheduled dispatch and reincarnate) and leaves Workspace empty.
func TestDeriveAgentConfig_HungStorageReturnsError(t *testing.T) {
	srv, _, _ := hungStorageServer(t)
	project := &store.Project{ID: "proj-derive-hung", Slug: "derive-hung", Name: "Derive Hung"}
	agent := &store.Agent{ID: "agent-derive-hung", AppliedConfig: &store.AgentAppliedConfig{}}

	err := srv.deriveAgentConfig(context.Background(), agent, project, nil)
	require.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.Empty(t, agent.AppliedConfig.Workspace)
}

// B1 (round 3): agent create with a caller-supplied workspace subdir. The
// first probe happens in the remote-broker upload branch, after the agent
// row exists. A storage timeout there must clean up the agent and answer
// 503, not dispatch without the upload (the broker would resolve the subdir
// against its own stale project copy).
func TestCreateAgent_CallerWorkspace_HungStorageReturns503NoAgentNoDispatch(t *testing.T) {
	srv, s, mountRoot := hungStorageServer(t)
	ctx := context.Background()
	disp := &mockDispatcher{}
	srv.SetDispatcher(disp)
	srv.SetStorage(newMockStorage("hung-storage-bucket"))

	broker := &store.RuntimeBroker{
		ID: tid("broker-hung-caller-ws"), Slug: "hung-caller-ws-broker",
		Name: "Hung Caller WS Broker", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("project-hung-caller-ws"), Slug: "hung-caller-ws", Name: "Hung Caller WS Project"}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name":            "hung-caller-ws-agent",
		"projectId":       project.ID,
		"runtimeBrokerId": broker.ID,
		"workspace":       "subdir",
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")

	agents, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "the agent row must be cleaned up")
	assert.Empty(t, disp.dispatchedAgents, "nothing may be dispatched")
}

// N1 (round 3): a rolled-back project create releases its
// max_projects_per_user reservation, so a hung mount does not leak quota
// slots on every retry.
func TestCreateProject_HungStorageReleasesProjectQuota(t *testing.T) {
	srv, s, _ := hungStorageServer(t)
	setUserProjectQuotaCeiling(t, s, 1)
	require.Zero(t, countProjectQuotaReservations(t, s, DevUserID))

	for i := 0; i < 2; i++ {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: fmt.Sprintf("Hung Quota %d", i)})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
		assert.Zero(t, countProjectQuotaReservations(t, s, DevUserID), "rollback must release the reservation")
	}

	// Storage healthy again: the single slot is still free.
	srv.config.WorkspaceStorageConfig = nil
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "Healthy Quota"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, int64(1), countProjectQuotaReservations(t, s, DevUserID), "the quota is enforced in this test")
}

// N3 (round 3): reincarnate (dry run) answers 503 when the agent has no
// workspace and the project workspace path cannot be resolved.
func TestReincarnateAgent_HungStorageReturns503(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	mountRoot := filepath.Join(tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)

	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Workspace = ""
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), mountRoot, "response must not leak the path")
	assert.Contains(t, rec.Body.String(), "Workspace storage is not responding")
}
