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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangReadDirFor makes workspaceReadDir block for any path under hungPrefix
// until the test ends, and shortens workspaceContentTimeout. Other paths use
// os.ReadDir. Tests in this package do not run in parallel, so mutating the
// package-level seams is safe.
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
	logs := captureSlog(t)

	start := time.Now()
	has, err := probeWorkspaceContent(dir)
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, errWorkspaceContentTimeout)
	assert.False(t, has)
	assert.Less(t, elapsed, 5*time.Second, "probe must not block on a hung read")
	assert.Contains(t, logs.String(), "Workspace content check timed out")
	assert.Contains(t, logs.String(), "path="+dir)

	assert.False(t, hasWorkspaceContent(dir), "a timed-out read counts as no content")
}

// A hung NFS mount must not send the project to the legacy local path, even
// when the local path has content: the durable path is returned.
func TestServerHubManagedProjectPath_NFSHungMountDoesNotFallBackToLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "hung-project"
	localDir := filepath.Join(tmpHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))

	mountRoot := filepath.Join(tmpHome, "nfs-mount")
	hangReadDirFor(t, mountRoot)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mountRoot, "share1", "hub-projects", slug), path)
}

// Same guarantee for the volume-backed (Cloud Run / GKE) branch.
func TestServerHubManagedProjectPath_VolumeHungMountDoesNotFallBackToLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "hung-vol-project"
	localDir := filepath.Join(tmpHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))

	mountBase := filepath.Join(tmpHome, "mnt")
	setVolumeMountBase(t, mountBase)
	hangReadDirFor(t, mountBase)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{
			VolumeName: "workspace-vol",
		},
	}

	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mountBase, "workspace-vol", "projects", "hub-projects", slug), path)
}
