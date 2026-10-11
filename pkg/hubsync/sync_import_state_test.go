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

// importTestProject creates a project directory for an import test and
// returns it with its resolved path and name, as the import compares them.
func importTestProject(t *testing.T) (projectDir, resolved, name string) {
	t.Helper()
	projectDir = filepath.Join(t.TempDir(), "importproj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	resolved = projectDir
	if pd, err := config.GetResolvedProjectDir(projectDir); err == nil && pd != "" {
		resolved = pd
	}
	return projectDir, resolved, config.GetProjectName(resolved)
}

// runImport runs ExecuteSync for agents against a fake hub and runtime and
// returns the create requests, the prompt output and the warnings.
func runImport(t *testing.T, projectDir string, list func() ([]api.AgentInfo, error), agents ...string) (reqs []hubclient.CreateAgentRequest, out, warn string, listCalls int, filters []map[string]string) {
	t.Helper()
	const projectID = "project-import-state"
	withImportRuntime(t, &runtime.MockRuntime{
		ListFunc: func(_ context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			listCalls++
			filters = append(filters, labelFilter)
			return list()
		},
	})
	outBuf, warnBuf := captureImportOutput(t)
	hub := &importStateHub{}
	srv := httptest.NewServer(hub.handler(t, projectID))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID, ProjectPath: projectDir, Settings: &config.Settings{}}

	require.NoError(t, ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: agents}, true))
	return hub.requests, outBuf.String(), warnBuf.String(), listCalls, filters
}

// TestExecuteSync_ImportKeepsNonRunningAgentsStopped checks that the import
// creates and starts only agents whose container in this project is
// running, and provisions every other agent without starting it.
func TestExecuteSync_ImportKeepsNonRunningAgentsStopped(t *testing.T) {
	projectDir, resolved, projectName := importTestProject(t)
	inProject := func(name, phase string) api.AgentInfo {
		return api.AgentInfo{Name: name, Phase: phase, ProjectPath: resolved, Labels: map[string]string{"scion.name": name}}
	}

	cases := []struct {
		name          string
		containers    []api.AgentInfo
		listErr       error
		wantProvision bool
		wantLine      string
		wantWarning   bool
	}{
		{name: "running container is adopted", containers: []api.AgentInfo{inProject("a1", "running")}, wantProvision: false, wantLine: "+ a1 (running, adopted)\n"},
		{name: "running container matched by project name label", containers: []api.AgentInfo{{Name: "a1", Phase: "running", Project: projectName, Labels: map[string]string{"scion.name": "a1"}}}, wantProvision: false, wantLine: "+ a1 (running, adopted)\n"},
		{name: "exited container is imported as stopped", containers: []api.AgentInfo{inProject("a1", "stopped")}, wantProvision: true, wantLine: "+ a1 (imported as stopped)\n"},
		{name: "no container is imported as created", wantProvision: true, wantLine: "+ a1 (imported as created)\n"},
		{name: "running same-name agent in another project does not count", containers: []api.AgentInfo{
			{Name: "a1", Phase: "running", ProjectPath: "/elsewhere/other/.scion", Project: projectName, Labels: map[string]string{"scion.name": "a1"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created)\n"},
		{name: "running same-name agent with another project name does not count", containers: []api.AgentInfo{
			{Name: "a1", Phase: "running", Project: "some-other-project", Labels: map[string]string{"scion.name": "a1"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created)\n"},
		{name: "running container without project labels does not count", containers: []api.AgentInfo{
			{Name: "a1", Phase: "running", Labels: map[string]string{"scion.name": "a1"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created)\n"},
		{name: "runtime error does not start the agent", listErr: errors.New("daemon not reachable"), wantProvision: true, wantLine: "+ a1 (imported, not started)\n", wantWarning: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, out, warn, calls, filters := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
				return tc.containers, tc.listErr
			}, "a1")

			assert.Equal(t, 1, calls)
			assert.Equal(t, []map[string]string{{"scion.agent": "true"}}, filters)
			require.Len(t, reqs, 1)
			assert.Equal(t, "a1", reqs[0].Name)
			assert.Equal(t, tc.wantProvision, reqs[0].ProvisionOnly)
			assert.Contains(t, out, tc.wantLine)
			if tc.wantWarning {
				assert.Contains(t, warn, "could not check which agents are running")
				assert.Contains(t, warn, "daemon not reachable")
			} else {
				assert.Empty(t, warn)
			}
		})
	}
}

// TestExecuteSync_ImportMixedAgents imports several agents in one sync and
// checks that the runtime is listed once and each request gets its own
// decision.
func TestExecuteSync_ImportMixedAgents(t *testing.T) {
	projectDir, resolved, _ := importTestProject(t)
	reqs, out, _, calls, _ := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{Name: "run", Phase: "running", ProjectPath: resolved, Labels: map[string]string{"scion.name": "run"}},
			{Name: "off", Phase: "stopped", ProjectPath: resolved, Labels: map[string]string{"scion.name": "off"}},
			{Name: "idle", Phase: "running", ProjectPath: "/elsewhere/.scion", Labels: map[string]string{"scion.name": "idle"}},
		}, nil
	}, "idle", "run", "off")

	assert.Equal(t, 1, calls, "the runtime is listed once for the whole import")
	require.Len(t, reqs, 3)
	assert.True(t, reqs[0].ProvisionOnly, "idle agent is provisioned only")
	assert.False(t, reqs[1].ProvisionOnly, "running agent keeps create and start")
	assert.True(t, reqs[2].ProvisionOnly, "stopped agent is provisioned only")
	assert.Equal(t, "+ idle (imported as created)\n+ run (running, adopted)\n+ off (imported as stopped)\nAgent synchronization complete.\n", out)
}

// TestExecuteSync_NoRegistrationsSkipsRuntime checks that a sync with
// nothing to register does not query the runtime.
func TestExecuteSync_NoRegistrationsSkipsRuntime(t *testing.T) {
	projectDir, _, _ := importTestProject(t)
	_, _, _, calls, _ := runImport(t, projectDir, func() ([]api.AgentInfo, error) { return nil, nil })
	assert.Equal(t, 0, calls)
}
