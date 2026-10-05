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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provideHub is an httptest hub that serves the project providers API for
// one project and records every request it receives.
type provideHub struct {
	mu        sync.Mutex
	requests  []string
	added     []hubclient.AddProviderRequest
	providers []hubclient.ProjectProvider
}

func newProvideHub(t *testing.T, projectID string, providers []hubclient.ProjectProvider) (*provideHub, *httptest.Server) {
	t.Helper()
	h := &provideHub{providers: providers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/providers":
			_ = json.NewEncoder(w).Encode(hubclient.ListProvidersResponse{Providers: h.providers})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/providers":
			var req hubclient.AddProviderRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			h.added = append(h.added, req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.AddProviderResponse{
				Provider: &hubclient.ProjectProvider{BrokerID: req.BrokerID, LocalPath: req.LocalPath},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "provide-project", DefaultRuntimeBrokerID: "broker-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func TestProvideBrokerToProject_UsesProvidersAPI(t *testing.T) {
	hub, srv := newProvideHub(t, "proj-1", nil)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	project, err := provideBrokerToProject(context.Background(), client, "proj-1", "broker-1", "/home/dev/repo")

	require.NoError(t, err)
	assert.Equal(t, "proj-1", project.ID)
	assert.Equal(t, "broker-1", project.DefaultRuntimeBrokerID)
	assert.Equal(t, []hubclient.AddProviderRequest{{BrokerID: "broker-1", LocalPath: "/home/dev/repo"}}, hub.added)
	for _, req := range hub.requests {
		assert.NotContains(t, req, "/projects/register", "provide links through the providers API")
	}
	assert.Contains(t, hub.requests, "POST /api/v1/projects/proj-1/providers")
}

func TestProvideBrokerToProject_KeepsExistingProviderLocalPath(t *testing.T) {
	hub, srv := newProvideHub(t, "proj-2", []hubclient.ProjectProvider{
		{BrokerID: "broker-1", LocalPath: ""},
	})
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = provideBrokerToProject(context.Background(), client, "proj-2", "broker-1", "/home/dev/repo")

	require.NoError(t, err)
	assert.Equal(t, []hubclient.AddProviderRequest{{BrokerID: "broker-1"}}, hub.added,
		"an existing provider's empty local path is kept")
}

func TestProvideBrokerToProject_ReturnsAddError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"only the broker's owner or a super-admin may associate this broker with a project"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.ListProvidersResponse{})
	}))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = provideBrokerToProject(context.Background(), client, "proj-3", "broker-1", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")
}
