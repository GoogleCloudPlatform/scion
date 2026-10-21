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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const credentialRegistry = "user:pa/ss@registry.example/team"

// TestRuntimeBrokerList_ProjectFilterRedactsNodeImageRegistry: the
// projectId-filtered broker list shows a reported node image registry with
// its credentials redacted (ptone/scion#3916); the stored value is kept.
func TestRuntimeBrokerList_ProjectFilterRedactsNodeImageRegistry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("redact-list-project"), Name: "Redact List", Slug: "redact-list", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("redact-list-broker"), Name: "Redact List Broker", Slug: "redact-list-broker", Status: store.BrokerStatusOnline,
		LastHeartbeat: time.Now(), Created: time.Now(), Updated: time.Now(),
		Capabilities: &store.BrokerCapabilities{NodeImageRegistry: credentialRegistry},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/runtime-brokers?projectId=%s", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListRuntimeBrokersWithProviderResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Brokers, 1)
	require.NotNil(t, resp.Brokers[0].Capabilities)
	assert.Equal(t, "***@registry.example/team", resp.Brokers[0].Capabilities.NodeImageRegistry)

	stored, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, credentialRegistry, stored.Capabilities.NodeImageRegistry, "the stored value is unchanged")
}

// TestRegisterProject_EmbeddedBrokerResponseRedactsNodeImageRegistry: the
// project registration response shows the registered broker's node image
// registry redacted; the stored value is kept.
func TestRegisterProject_EmbeddedBrokerResponseRedactsNodeImageRegistry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	const brokerName = "redact-register-broker"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "redact-register-project",
		GitRemote: "github.com/org/redact-register",
		Broker: &RegisterProjectBrokerInfo{
			Name:         brokerName,
			Version:      "1.0.0",
			Capabilities: &store.BrokerCapabilities{NodeImageRegistry: credentialRegistry},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Broker)
	require.NotNil(t, resp.Broker.Capabilities)
	assert.Equal(t, "***@registry.example/team", resp.Broker.Capabilities.NodeImageRegistry)

	stored, err := s.GetRuntimeBrokerByName(ctx, brokerName)
	require.NoError(t, err)
	require.NotNil(t, stored.Capabilities)
	assert.Equal(t, credentialRegistry, stored.Capabilities.NodeImageRegistry, "the stored value is unchanged")
}
