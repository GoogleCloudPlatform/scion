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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// importStateHub is a fake hub that records each agent create request.
type importStateHub struct {
	requests []hubclient.CreateAgentRequest
}

func (h *importStateHub) handler(t *testing.T, projectID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/"+projectID+"/agents" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req hubclient.CreateAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode create request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.requests = append(h.requests, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agent": map[string]string{"id": "id-" + req.Name, "name": req.Name},
		})
	}
}

// withImportRuntime replaces the runtime the import queries for the test.
func withImportRuntime(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	prev := importRuntime
	importRuntime = func(string) runtime.Runtime { return rt }
	t.Cleanup(func() { importRuntime = prev })
}

// captureImportOutput sends prompt and warning output to buffers.
func captureImportOutput(t *testing.T) (out, warn *bytes.Buffer) {
	t.Helper()
	out, warn = &bytes.Buffer{}, &bytes.Buffer{}
	prevOut, prevWarn := promptOut, warnOut
	promptOut, warnOut = out, warn
	t.Cleanup(func() { promptOut, warnOut = prevOut, prevWarn })
	return out, warn
}

// TestExecuteSync_ImportKeepsNonRunningAgentsStopped checks that the import
// creates and starts only agents whose container is running, and provisions
// every other agent without starting it.
func TestExecuteSync_ImportKeepsNonRunningAgentsStopped(t *testing.T) {
	const projectID = "project-import-state"

	// Containers on the host, keyed by the scion.name label filter.
	containers := map[string][]api.AgentInfo{
		"a1": {{Name: "a1", Phase: "running", Labels: map[string]string{"scion.name": "a1"}}},
		"b1": {{Name: "b1", Phase: "stopped", ContainerStatus: "Exited (0) 2 hours ago", Labels: map[string]string{"scion.name": "b1"}}},
		// c1 has no container (created only).
	}

	cases := []struct {
		name          string
		agent         string
		listErr       error
		wantProvision bool
		wantLine      string
		wantWarning   bool
	}{
		{name: "running container is adopted", agent: "a1", wantProvision: false, wantLine: "+ a1 (running, adopted)\n"},
		{name: "exited container is imported as stopped", agent: "b1", wantProvision: true, wantLine: "+ b1 (imported as stopped)\n"},
		{name: "no container is imported as created", agent: "c1", wantProvision: true, wantLine: "+ c1 (imported as created)\n"},
		{name: "runtime error does not start the agent", agent: "d1", listErr: errors.New("daemon not reachable"), wantProvision: true, wantLine: "+ d1 (imported, not started)\n", wantWarning: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotFilter map[string]string
			withImportRuntime(t, &runtime.MockRuntime{
				ListFunc: func(_ context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					gotFilter = labelFilter
					if tc.listErr != nil {
						return nil, tc.listErr
					}
					return containers[labelFilter["scion.name"]], nil
				},
			})
			out, warn := captureImportOutput(t)

			hub := &importStateHub{}
			srv := httptest.NewServer(hub.handler(t, projectID))
			defer srv.Close()
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			projectDir := filepath.Join(t.TempDir(), ".scion")
			require.NoError(t, os.MkdirAll(projectDir, 0755))
			hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID, ProjectPath: projectDir, Settings: &config.Settings{}}

			require.NoError(t, ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: []string{tc.agent}}, true))

			assert.Equal(t, map[string]string{"scion.agent": "true", "scion.name": tc.agent}, gotFilter)
			require.Len(t, hub.requests, 1)
			assert.Equal(t, tc.agent, hub.requests[0].Name)
			assert.Equal(t, tc.wantProvision, hub.requests[0].ProvisionOnly)
			assert.Contains(t, out.String(), tc.wantLine)
			if tc.wantWarning {
				assert.Contains(t, warn.String(), "could not check whether agent '"+tc.agent+"' is running")
				assert.Contains(t, warn.String(), "daemon not reachable")
			} else {
				assert.Empty(t, warn.String())
			}
		})
	}
}

// TestExecuteSync_ImportMixedAgents imports several agents in one sync and
// checks that each request gets its own decision.
func TestExecuteSync_ImportMixedAgents(t *testing.T) {
	const projectID = "project-import-mixed"
	withImportRuntime(t, &runtime.MockRuntime{
		ListFunc: func(_ context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			if labelFilter["scion.name"] == "run" {
				return []api.AgentInfo{{Name: "run", Phase: "running"}}, nil
			}
			return nil, nil
		},
	})
	out, _ := captureImportOutput(t)

	hub := &importStateHub{}
	srv := httptest.NewServer(hub.handler(t, projectID))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID, ProjectPath: projectDir, Settings: &config.Settings{}}

	require.NoError(t, ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: []string{"idle", "run"}}, true))

	require.Len(t, hub.requests, 2)
	assert.True(t, hub.requests[0].ProvisionOnly, "idle agent is provisioned only")
	assert.False(t, hub.requests[1].ProvisionOnly, "running agent keeps create and start")
	assert.Equal(t, "+ idle (imported as created)\n+ run (running, adopted)\nAgent synchronization complete.\n", out.String())
}
