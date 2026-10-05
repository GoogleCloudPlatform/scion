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
