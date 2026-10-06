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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	_, err := registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, first)
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
	_, err = registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, second)
	require.NoError(t, err)
	b, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, second, b.WorkspaceStorage, "re-registration refreshes the descriptor")

	_, err = registerGlobalProjectAndBroker(ctx, s, brokerID, "ws-register-broker", "http://localhost:9800", rt, true, settings, nil)
	require.NoError(t, err)
	b, err = s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.Equal(t, second, b.WorkspaceStorage, "an unreadable descriptor (nil) keeps the stored one")
}

func TestBrokerRegistrationWorkspaceStorage(t *testing.T) {
	assert.Equal(t, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendLocal},
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
