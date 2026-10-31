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
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storageHealth runs the workspace_storage check, GET /readyz and GET /healthz
// on e's server and returns the check value, the readyz code and body, and
// the healthz status and workspace_storage value.
type storageHealth struct {
	check         string
	readyzCode    int
	readyzBody    string
	healthzStatus string
	healthzCheck  string
}

func (e *storageCleanupEnv) storageHealth(t *testing.T) storageHealth {
	t.Helper()
	var out storageHealth
	checks := map[string]string{}
	e.srv.checkWorkspaceStorageHealth(checks)
	out.check = checks["workspace_storage"]
	rec := doRequest(t, e.srv, http.MethodGet, "/readyz", nil)
	out.readyzCode, out.readyzBody = rec.Code, rec.Body.String()
	rec = doRequest(t, e.srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var hz struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hz), rec.Body.String())
	out.healthzStatus, out.healthzCheck = hz.Status, hz.Checks["workspace_storage"]
	return out
}

func (e *storageCleanupEnv) addProject(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, e.s.CreateProject(context.Background(), &store.Project{ID: tid(name), Slug: name, Name: name}))
}

func assertNotReady(t *testing.T, h storageHealth, want string) {
	t.Helper()
	assert.Equal(t, want, h.check)
	assert.Equal(t, http.StatusServiceUnavailable, h.readyzCode, "readyz body: %s", h.readyzBody)
	assert.Contains(t, h.readyzBody, "workspace storage not available")
	assert.Equal(t, HealthStatusUnhealthy, h.healthzStatus)
	assert.Equal(t, want, h.healthzCheck)
}

func assertReady(t *testing.T, h storageHealth) {
	t.Helper()
	assert.Equal(t, HealthStatusHealthy, h.check)
	assert.Equal(t, http.StatusOK, h.readyzCode, "readyz body: %s", h.readyzBody)
	assert.Equal(t, HealthStatusHealthy, h.healthzCheck)
}

// An nfs Hub whose share is not mounted (the mount point is empty) while it
// has projects is not ready: workspace_storage is unhealthy, /readyz 503,
// /healthz unhealthy.
func TestWorkspaceStorageHealth_NFSEmptyMountPoint_NotReady(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.addProject(t, "ws-empty-p")
	require.NoError(t, os.RemoveAll(e.subRoot))
	assertNotReady(t, e.storageHealth(t), workspaceStorageShareEmpty)
}

// A mounted share without the configured sub-path root, on a Hub with
// projects, is not ready either, with its own fixed text.
func TestWorkspaceStorageHealth_NFSMissingSubPathRoot_NotReady(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.addProject(t, "ws-noroot-p")
	require.NoError(t, os.RemoveAll(e.subRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(e.share, "other-tree"), 0o755))
	assertNotReady(t, e.storageHealth(t), workspaceStorageNoTree)
}

// A share with its project tree is ready, and on nfs the tree is reported
// only through workspace_storage: there is no separate project_storage_tree
// key.
func TestWorkspaceStorageHealth_NFSHealthyShare_Ready(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.addProject(t, "ws-ok-p")
	assertReady(t, e.storageHealth(t))
	checks := e.srv.healthChecks(context.Background())
	_, has := checks[projectStorageTreeCheck]
	assert.False(t, has, "nfs reports the tree through workspace_storage only: %v", checks)
}

// A fresh Hub with no projects on an empty share stays ready: the project
// tree appears with the first project.
func TestWorkspaceStorageHealth_NFSFreshHubEmptyShare_Ready(t *testing.T) {
	e := newStorageCleanupEnv(t)
	require.NoError(t, os.RemoveAll(e.subRoot))
	entries, err := os.ReadDir(e.share)
	require.NoError(t, err)
	require.Empty(t, entries, "the share is empty")
	assertReady(t, e.storageHealth(t))
}

// A hung tree probe is bounded by workspaceHealthTimeout and reads as not
// ready; repeated checks join the one in-flight probe instead of starting
// another.
func TestWorkspaceStorageHealth_NFSHungTreeProbe_BoundedSingleInFlight(t *testing.T) {
	e := newStorageCleanupEnv(t)
	e.addProject(t, "ws-hung-p")
	var calls atomic.Int32
	release := make(chan struct{})
	prevProbe, prevTimeout := projectTreeHealthProbe, workspaceHealthTimeout
	projectTreeHealthProbe = func(projectfs.Layout) error { calls.Add(1); <-release; return nil }
	workspaceHealthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { close(release); projectTreeHealthProbe, workspaceHealthTimeout = prevProbe, prevTimeout })

	for i := 0; i < 3; i++ {
		start := time.Now()
		checks := map[string]string{}
		e.srv.checkWorkspaceStorageHealth(checks)
		assert.Less(t, time.Since(start), 2*time.Second, "the check is bounded")
		assert.Equal(t, workspaceStorageTreeTimedOut, checks["workspace_storage"])
	}
	assert.Equal(t, int32(1), calls.Load(), "one in-flight tree probe across repeated checks")
	rec := doRequest(t, e.srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, int32(1), calls.Load(), "readyz joins the same in-flight probe")
}

// The workspace_storage values and the /readyz body never carry the host
// base, the mount root or the configured sub-path value (/healthz and
// /readyz are unauthenticated).
func TestWorkspaceStorageHealth_NFSTextsArePathFree(t *testing.T) {
	e := newStorageCleanupEnv(t)
	const subPath = "zq7-subpath-value"
	e.srv.config.ProjectStorageConfig.NFS.SubPathRoot = subPath
	subRoot := filepath.Join(e.share, subPath)
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	require.NoError(t, os.RemoveAll(e.subRoot))
	e.addProject(t, "ws-text-p")
	mountRoot := e.srv.config.ProjectStorageConfig.NFS.MountRoot
	forbidden := []string{e.share, mountRoot, subPath, e.home}

	var seen []string
	collect := func() {
		h := e.storageHealth(t)
		seen = append(seen, h.check, h.readyzBody, h.healthzCheck)
	}
	collect() // healthy
	require.NoError(t, os.RemoveAll(subRoot))
	collect() // empty or not mounted
	require.NoError(t, os.MkdirAll(filepath.Join(e.share, "other"), 0o755))
	collect() // no project tree
	prevProbe := projectTreeHealthProbe
	projectTreeHealthProbe = func(projectfs.Layout) error { return errors.New("probe failed at " + subRoot) }
	t.Cleanup(func() { projectTreeHealthProbe = prevProbe })
	collect() // could not be checked
	assert.Contains(t, seen, workspaceStorageTreeError)
	for _, v := range seen {
		for _, f := range forbidden {
			assert.NotContains(t, v, f)
		}
	}
}

// listProjectsFaultStore fails ListProjects while its switch is armed.
type listProjectsFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

var errListProjectsFault = errors.New("injected list projects fault")

func (w *listProjectsFaultStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	if !w.fault.Active() {
		return w.Store.ListProjects(ctx, f, o)
	}
	return nil, errListProjectsFault
}

// A store error while deciding whether the Hub has projects fails closed: an
// empty share then reads as not mounted, not as a fresh Hub.
func TestWorkspaceStorageHealth_NFSStoreErrorFailsClosed(t *testing.T) {
	e := newStorageCleanupEnv(t)
	_, fault := installStoreFault(t, e.srv, func(inner store.Store, f *storeFaultSwitch) *listProjectsFaultStore {
		return &listProjectsFaultStore{Store: inner, fault: f}
	})
	require.NoError(t, os.RemoveAll(e.subRoot))
	fault.Arm()
	checks := map[string]string{}
	e.srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, workspaceStorageShareEmpty, checks["workspace_storage"])
}

// The shared backends other than nfs keep the non-critical
// project_storage_tree key, and a missing tree there leaves the Hub ready.
func TestWorkspaceStorageHealth_CloudRunVolumeKeepsProjectStorageTree(t *testing.T) {
	e := newStorageCleanupEnv(t)
	prev := volumeMountBase
	volumeMountBase = t.TempDir()
	t.Cleanup(func() { volumeMountBase = prev })
	require.NoError(t, os.MkdirAll(filepath.Join(volumeMountBase, "vol"), 0o755))
	e.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "vol"},
	}
	e.addProject(t, "ws-crv-p")
	checks := e.srv.healthChecks(context.Background())
	assert.Equal(t, HealthStatusHealthy, checks["workspace_storage"])
	assert.True(t, strings.HasPrefix(checks[projectStorageTreeCheck], "degraded:"), "got %q", checks[projectStorageTreeCheck])
	assert.False(t, criticalHealthChecks[projectStorageTreeCheck])
	rec := doRequest(t, e.srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
