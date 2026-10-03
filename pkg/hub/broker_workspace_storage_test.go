//go:build !no_sqlite

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

package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleteBrokerJoin_StoresWorkspaceStorageAndAgentMove(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	desc := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol1", SubPathRoot: "projects"},
	}
	for _, tc := range []struct {
		name      string
		desc      *api.BrokerWorkspaceStorage
		caps      []string
		agentMove bool
	}{
		{name: "ws-join-with-descriptor", desc: desc, caps: []string{"sync", "agentMove"}, agentMove: true},
		{name: "ws-join-old-broker", desc: nil, caps: []string{"sync"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := svc.CreateBrokerRegistration(ctx, CreateBrokerRegistrationRequest{Name: tc.name}, "admin-user-id")
			require.NoError(t, err)
			_, err = svc.CompleteBrokerJoin(ctx, BrokerJoinRequest{
				BrokerID:         reg.BrokerID,
				JoinToken:        reg.JoinToken,
				Hostname:         tc.name,
				Version:          "1.0.0",
				Capabilities:     tc.caps,
				WorkspaceStorage: tc.desc,
			}, "http://localhost:9810")
			require.NoError(t, err)

			b, err := s.GetRuntimeBroker(ctx, reg.BrokerID)
			require.NoError(t, err)
			assert.Equal(t, tc.desc, b.WorkspaceStorage)
			require.NotNil(t, b.Capabilities)
			assert.Equal(t, tc.agentMove, b.Capabilities.AgentMove)
		})
	}
}

func TestCapabilitiesFromStrings_AgentMoveAliases(t *testing.T) {
	for _, name := range []string{"agentmove", "agentMove", "agent_move", " AGENT_MOVE "} {
		assert.True(t, capabilitiesFromStrings([]string{name}).AgentMove, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"sync"}).AgentMove)
}

func TestBrokerHeartbeat_WorkspaceStorageRoundTrip(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-ws-heartbeat"),
		Name:   "WS Heartbeat Broker",
		Slug:   "ws-heartbeat-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	unhealthy := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol1", SubPathRoot: "projects"},
	}
	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online", WorkspaceStorage: unhealthy})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, unhealthy, got.WorkspaceStorage, "heartbeat stores the descriptor")

	healthy := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol1", SubPathRoot: "projects", Healthy: true},
	}
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status:           "online",
		Capabilities:     &store.BrokerCapabilities{Reprovision: true},
		WorkspaceStorage: healthy,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, healthy, got.WorkspaceStorage, "heartbeat refreshes share health")
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Reprovision, "capabilities refresh alongside the descriptor")

	// An old broker's heartbeat has no descriptor: the stored one stays.
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{Status: "online"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, healthy, got.WorkspaceStorage, "a heartbeat without a descriptor leaves it untouched")
}
