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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// brokerListLocalPath is the provider LocalPath stored in these tests.
const brokerListLocalPath = "/srv/broker-list-local-path"

// decodeBrokerListLocalPaths decodes a project-filtered runtime broker
// listing into broker ID -> LocalPath.
func decodeBrokerListLocalPaths(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListRuntimeBrokersWithProviderResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	out := make(map[string]string, len(resp.Brokers))
	for _, b := range resp.Brokers {
		out[b.ID] = b.LocalPath
	}
	return out
}

// TestListRuntimeBrokers_LocalPathOnlyForProviderReaders: a project-filtered
// listing attaches the provider LocalPath only for a caller who may read the
// project's provider records (ptone/scion#6844).
func TestListRuntimeBrokers_LocalPathOnlyForProviderReaders(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	ctx := context.Background()

	// Bob owns the broker, so he may read it, but he may not read the
	// project it serves.
	broker := &store.RuntimeBroker{
		ID: tid("bll-broker"), Name: "bll-broker", Slug: "bll-broker", Status: store.BrokerStatusOnline,
		CreatedBy: bob.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: brokerListLocalPath,
	}))
	path := "/api/v1/runtime-brokers?projectId=" + project.ID

	t.Run("project reader sees the LocalPath", func(t *testing.T) {
		got := decodeBrokerListLocalPaths(t, doRequestAsUser(t, srv, alice, http.MethodGet, path, nil))
		require.Contains(t, got, broker.ID, "fixture: alice lists the broker")
		assert.Equal(t, brokerListLocalPath, got[broker.ID])
	})

	t.Run("non-reader gets the broker without the LocalPath", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, path, nil)
		assert.NotContains(t, rec.Body.String(), brokerListLocalPath)
		got := decodeBrokerListLocalPaths(t, rec)
		require.Contains(t, got, broker.ID, "fixture: bob lists his own broker, so the check is not vacuous")
		assert.Empty(t, got[broker.ID])
	})

	// Agent callers: a runtime broker is a hub-level target
	// (authz_boundary.go, the broker branch of ResolveTargetScope), which a
	// project-bounded agent token may not read (authz_bearer.go,
	// bearerReasonHubLevelResource), so the listing filters the broker out
	// and the agent never reaches the LocalPath gate here. The gate's agent
	// branch is tested in TestCallerMayReadProviders_GoodStandingAgent. If
	// agents may ever list brokers, this subtest fails and must assert the
	// gate's outcome instead. The absence is hub-scope confinement, not the
	// LocalPath gate.
	for _, tc := range []struct {
		name, agent string
		held        bool
	}{
		{"good-standing same-project agent", "bll-ok-agent", false},
		{"held same-project agent", "bll-held-agent", true},
	} {
		t.Run(tc.name+" does not list the broker or see the LocalPath", func(t *testing.T) {
			agentID := createProjectAgentForGate(t, srv, s, project.ID, alice.ID, tc.agent, tc.held)
			rec := doRequestAsIdentity(t, srv, newTestAgentIdentity(agentID, project.ID, ScopesForRole(AgentRoleFull)), http.MethodGet, path, nil)
			assert.NotContains(t, rec.Body.String(), brokerListLocalPath)
			got := decodeBrokerListLocalPaths(t, rec) // requires 200
			assert.NotContains(t, got, broker.ID, "a hub-level broker is not listed to a project-bounded agent")
		})
	}

	t.Run("without projectId the response is unchanged", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/runtime-brokers", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListRuntimeBrokersResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		found := false
		for _, b := range resp.Brokers {
			found = found || b.ID == broker.ID
		}
		assert.True(t, found, "alice lists the broker without a project filter")
		assert.NotContains(t, rec.Body.String(), brokerListLocalPath, "the unfiltered listing never carried LocalPath")
	})
}
