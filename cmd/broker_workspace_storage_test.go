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

package cmd

import (
	"context"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterGlobalProjectAndBroker_WorkspaceStorage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	brokerID := tid("broker-ws-register")

	first := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol1", SubPathRoot: "projects"},
	}
	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, first, nil)
	require.NoError(t, err)
	b, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, first, b.WorkspaceStorage, "create stores the descriptor")
	require.NotNil(t, b.Capabilities)
	assert.True(t, b.Capabilities.AgentMove, "this broker binary supports agent move")

	second := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.3", Export: "/vol2", SubPathRoot: "projects"},
	}
	_, err = registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, second, nil)
	require.NoError(t, err)
	b, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, second, b.WorkspaceStorage, "re-registration refreshes the descriptor")

	_, err = registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, nil, nil)
	require.NoError(t, err)
	b, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, second, b.WorkspaceStorage, "an unreadable descriptor (nil) keeps the stored one")
}

func TestBrokerRegistrationWorkspaceStorage(t *testing.T) {
	assert.Equal(t, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal, Homes: "local"},
		brokerRegistrationWorkspaceStorage(nil), "no settings means local")

	vs := &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: "/mnt/scion-nfs", Shares: []config.V1NFSShare{
			{ID: "a", Server: "10.0.0.2", Export: "/vol-a"},
			{ID: "b", Server: "10.0.0.9", Export: "/vol-b"},
		}},
	}}}
	got := brokerRegistrationWorkspaceStorage(vs)
	require.NotNil(t, got.NFS)
	assert.Equal(t, api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol-a", SubPathRoot: "projects"}, *got.NFS,
		"first share, defaults applied, unhealthy until the broker checks the mount")

	invalid := &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: &config.V1WorkspaceStorageConfig{Backend: "nfs"}}}
	got = brokerRegistrationWorkspaceStorage(invalid)
	assert.Equal(t, api.WorkspaceStorageBackendNFS, got.Backend)
	assert.Nil(t, got.NFS, "invalid nfs config reports no share")
}

// TestBrokerStorageLayout: the broker builds its Layout from the same
// server.project_storage struct as the hub (ptone/scion#5823), reports its
// homes, and a settings file with only project_storage places workspaces
// exactly as the equivalent workspace_storage block did.
func TestBrokerStorageLayout(t *testing.T) {
	ps := &config.VersionedSettings{Server: &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{
		Backend: "nfs", Homes: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: "/mnt/scion-nfs", Shares: []config.V1NFSShare{{ID: "a", Server: "10.0.0.2", Export: "/vol-a", PVName: "pv-a"}}},
	}}}
	layout, warnings, storageErr := brokerStorageLayout(ps)
	require.NoError(t, storageErr)
	assert.Empty(t, warnings)
	assert.Equal(t, projectfs.BackendNFS, layout.Backend)
	assert.Equal(t, "/mnt/scion-nfs/a", layout.HostBase)
	assert.Equal(t, projectfs.BackendNFS, layout.Homes)

	got := brokerRegistrationWorkspaceStorage(ps)
	assert.Equal(t, "nfs", got.Homes)
	require.NotNil(t, got.NFS, "project_storage alone configures the workspace export")
	assert.Equal(t, "/vol-a", got.NFS.Export)
	assert.Empty(t, layout.DescriptorDiff(got, ""), "a broker's registration descriptor matches its own Layout")

	legacy := &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: &config.V1WorkspaceStorageConfig{
		Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt/scion-nfs", Shares: []config.V1NFSShare{{ID: "a", Server: "10.0.0.2", Export: "/vol-a"}}},
	}}}
	layout, warnings, storageErr = brokerStorageLayout(legacy)
	require.NoError(t, storageErr)
	assert.Equal(t, projectfs.BackendNFS, layout.Backend)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0], "legacy storage blocks in use")

	both := &config.VersionedSettings{Server: &config.V1ServerConfig{
		ProjectStorage:   &config.V1ProjectStorageConfig{Backend: "local"},
		WorkspaceStorage: &config.V1WorkspaceStorageConfig{Backend: "local"},
	}}
	layout, warnings, storageErr = brokerStorageLayout(both)
	assert.Equal(t, projectfs.Layout{}, layout, "an unusable block reports no layout (never the local one)")
	assert.Empty(t, warnings)
	require.Error(t, storageErr)
	assert.Contains(t, storageErr.Error(), "exactly one storage block")
}

// TestBrokerSharedDirLayoutWarnings: a shared-dir Layout that would fail
// Kubernetes starts is reported once at startup.
func TestBrokerSharedDirLayoutWarnings(t *testing.T) {
	missing := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	present := func(string) (os.FileInfo, error) { return nil, nil }
	assert.Empty(t, brokerSharedDirLayoutWarnings(projectfs.Layout{Backend: projectfs.BackendLocal}, missing))
	assert.Empty(t, brokerSharedDirLayoutWarnings(projectfs.Layout{Backend: projectfs.BackendNFS, Share: projectfs.Share{PVName: "pv"}}, missing))
	// No pv_name on the Layout's own nfs share: pods mount it inline, so
	// no warning (the startup INFO line says so); the legacy split still
	// needs its claim.
	assert.Empty(t, brokerSharedDirLayoutWarnings(projectfs.Layout{Backend: projectfs.BackendNFS}, missing))
	split := projectfs.Layout{Backend: projectfs.BackendLocal, SharedDirs: &projectfs.Layout{Backend: projectfs.BackendNFS}}
	got := brokerSharedDirLayoutWarnings(split, missing)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "pv_name")
	got = brokerSharedDirLayoutWarnings(projectfs.Layout{Backend: projectfs.BackendGKESharedVolume, HostBase: "/mnt/v"}, missing)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "pv_claim_name")
	assert.Contains(t, got[1], "not mounted on this broker at /mnt/v")
	assert.Empty(t, brokerSharedDirLayoutWarnings(projectfs.Layout{Backend: projectfs.BackendGKESharedVolume, Share: projectfs.Share{PVName: "c"}}, present))
}
