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

package hubsync

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

// ensureProviderPath sends the project's own path as the provider path.
func TestEnsureProviderPath_SendsProjectPath(t *testing.T) {
	const projectID = "p-web"
	var (
		mu    sync.Mutex
		added []map[string]interface{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects/"+projectID+"/providers" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		case r.URL.Path == "/api/v1/projects/"+projectID+"/providers" && r.Method == http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			added = append(added, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"provider": body})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	projectPath := "/home/brokeruser/src/web-app/.scion"
	require.NoError(t, ensureProviderPath(context.Background(), &HubContext{
		Client: client, ProjectID: projectID, BrokerID: "broker-1", ProjectPath: projectPath,
	}))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, added, 1)
	assert.Equal(t, "broker-1", added[0]["brokerId"])
	assert.Equal(t, projectPath, added[0]["localPath"])
}
