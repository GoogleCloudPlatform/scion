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
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/require"
)

// twoShareNFSConfig has two shares on different servers so a descriptor
// that picked the wrong share would be visible.
func twoShareNFSConfig() *config.V1NFSConfig {
	return &config.V1NFSConfig{
		MountRoot:   "/mnt/scion-nfs",
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "primary", Server: "10.0.0.2", Export: "/vol-a", PVName: "pv-primary"},
			{ID: "secondary", Server: "10.0.0.9", Export: "/vol-b", PVName: "pv-secondary"},
		},
	}
}

func TestBuildWorkspaceStorageDescriptor(t *testing.T) {
	healthyIDs := func(ids ...string) func(string) bool {
		return func(id string) bool {
			for _, want := range ids {
				if id == want {
					return true
				}
			}
			return false
		}
	}

	t.Run("unset backend is local", func(t *testing.T) {
		got := BuildWorkspaceStorageDescriptor("", nil, nil)
		require.Equal(t, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal}, got)
	})
	t.Run("local backend ignores nfs config", func(t *testing.T) {
		got := BuildWorkspaceStorageDescriptor("local", twoShareNFSConfig(), healthyIDs("primary"))
		require.Equal(t, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal}, got)
	})
	t.Run("nfs backend without valid config has no share", func(t *testing.T) {
		got := BuildWorkspaceStorageDescriptor("nfs", nil, nil)
		require.Equal(t, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendNFS}, got)
		got = BuildWorkspaceStorageDescriptor("nfs", &config.V1NFSConfig{}, nil)
		require.Nil(t, got.NFS)
	})
	t.Run("backend name is case-insensitive", func(t *testing.T) {
		got := BuildWorkspaceStorageDescriptor(" NFS ", twoShareNFSConfig(), nil)
		require.Equal(t, api.WorkspaceStorageBackendNFS, got.Backend)
		require.NotNil(t, got.NFS)
	})
	t.Run("unset subpath root reports the backend default", func(t *testing.T) {
		cfg := twoShareNFSConfig()
		cfg.SubPathRoot = ""
		got := BuildWorkspaceStorageDescriptor("nfs", cfg, nil)
		require.Equal(t, "projects", got.NFS.SubPathRoot)
	})
	t.Run("health follows the first share only", func(t *testing.T) {
		require.True(t, BuildWorkspaceStorageDescriptor("nfs", twoShareNFSConfig(), healthyIDs("primary")).NFS.Healthy)
		require.False(t, BuildWorkspaceStorageDescriptor("nfs", twoShareNFSConfig(), healthyIDs("secondary")).NFS.Healthy)
		require.False(t, BuildWorkspaceStorageDescriptor("nfs", twoShareNFSConfig(), nil).NFS.Healthy)
	})
}

// TestBuildWorkspaceStorageDescriptor_DescribesShareWorkspacesUse pins the
// descriptor to the share the NFS workspace backend actually places
// workspaces on: the hub's same-export comparison is only meaningful if
// both agree on Shares[0].
func TestBuildWorkspaceStorageDescriptor_DescribesShareWorkspacesUse(t *testing.T) {
	cfg := twoShareNFSConfig()
	desc := BuildWorkspaceStorageDescriptor("nfs", cfg, nil)
	require.NotNil(t, desc.NFS)
	require.Equal(t, "10.0.0.2", desc.NFS.Server)
	require.Equal(t, "/vol-a", desc.NFS.Export)
	require.Equal(t, cfg.SubPathRoot, desc.NFS.SubPathRoot)

	backend := runtime.NewNFSBackend(cfg)
	resolved, err := backend.Resolve(runtime.ResolveInput{ProjectID: "proj-1", AgentID: "agent-1"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(cfg.MountRoot, cfg.Shares[0].ID), resolved.HostBase,
		"workspaces are placed on Shares[0]")
	require.Equal(t, filepath.Join(desc.NFS.SubPathRoot, "proj-1", "workspace"), resolved.ServerRelativePath,
		"workspace path under the export uses the described subpath root")

	mount, err := backend.Realize(runtime.RealizeInput{Resolved: resolved})
	require.NoError(t, err)
	require.Equal(t, cfg.Shares[0].PVName, mount.PVClaimName)
}

func TestHeartbeat_ReportsWorkspaceStorageAndNoAgentMove(t *testing.T) {
	hb := NewHeartbeatService(&mockRuntimeBrokerService{}, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got := hb.buildHeartbeat(context.Background())
	require.NotNil(t, got.Capabilities)
	require.False(t, got.Capabilities.AgentMove)
	require.Nil(t, got.WorkspaceStorage, "no descriptor without a provider")

	want := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol-a", SubPathRoot: "projects", Healthy: true},
	}
	hb.workspaceStorage = func() *api.BrokerWorkspaceStorage { return want }
	got = hb.buildHeartbeat(context.Background())
	require.Equal(t, want, got.WorkspaceStorage)
	require.False(t, got.Capabilities.AgentMove)
}

func TestHubConnectionStart_HeartbeatReportsWorkspaceStorage(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)
	cfg := srv.config
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = false
	cfg.WorkspaceStorageBackend = "nfs"
	cfg.NFSConfig = twoShareNFSConfig()
	srv.config = cfg

	srv.hubMu.RLock()
	conn, ok := srv.hubConnections["local"]
	srv.hubMu.RUnlock()
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, conn.Start(ctx, srv))

	conn.mu.RLock()
	hb := conn.Heartbeat
	conn.mu.RUnlock()
	require.NotNil(t, hb)
	got := hb.buildHeartbeat(ctx)
	require.NotNil(t, got.WorkspaceStorage)
	require.Equal(t, api.WorkspaceStorageBackendNFS, got.WorkspaceStorage.Backend)
	require.NotNil(t, got.WorkspaceStorage.NFS)
	require.Equal(t, "10.0.0.2", got.WorkspaceStorage.NFS.Server)
	require.False(t, got.WorkspaceStorage.NFS.Healthy, "no mount reconciler has checked the share")
}
