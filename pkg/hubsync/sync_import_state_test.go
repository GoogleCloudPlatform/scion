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
	events   *[]string // shared with the fake runtime, to check ordering
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
		if h.events != nil {
			*h.events = append(*h.events, "create "+req.Name)
		}
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

// importRun is the result of one runImport call.
type importRun struct {
	reqs      []hubclient.CreateAgentRequest
	out, warn string
	listCalls int
	filters   []map[string]string
	deleted   []runtime.RunRef
	events    []string // "delete <id>" and "create <name>", in order
}

// runImport runs ExecuteSync for agents against a fake hub and runtime.
// deleteErr, when set, is returned by every runtime Delete.
func runImport(t *testing.T, projectDir string, list func() ([]api.AgentInfo, error), deleteErr error, agents ...string) importRun {
	t.Helper()
	const projectID = "project-import-state"
	var r importRun
	withImportRuntime(t, &runtime.MockRuntime{
		ListFunc: func(_ context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			r.listCalls++
			r.filters = append(r.filters, labelFilter)
			return list()
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			r.events = append(r.events, "delete "+ref.ID)
			if deleteErr != nil {
				return deleteErr
			}
			r.deleted = append(r.deleted, ref)
			return nil
		},
	})
	outBuf, warnBuf := captureImportOutput(t)
	hub := &importStateHub{events: &r.events}
	srv := httptest.NewServer(hub.handler(t, projectID))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID, ProjectPath: projectDir, Settings: &config.Settings{}}

	require.NoError(t, ExecuteSync(context.Background(), hubCtx, &SyncResult{ToRegister: agents}, true))
	r.reqs, r.out, r.warn = hub.requests, outBuf.String(), warnBuf.String()
	return r
}

// localContainer builds a container of agent name in the project at path,
// labelled as a local start labels it.
func localContainer(id, name, phase, path string) api.AgentInfo {
	labels := map[string]string{"scion.agent": "true", "scion.name": name}
	if path != "" {
		labels["scion.project_path"] = path
	}
	return api.AgentInfo{ContainerID: id, RunID: "run-" + id, Name: name, Phase: phase, ProjectPath: path, Labels: labels}
}

// TestExecuteSync_ImportKeepsNonRunningAgentsStopped checks that the import
// creates and starts only agents whose container in this project is
// running, and provisions every other agent without starting it.
func TestExecuteSync_ImportKeepsNonRunningAgentsStopped(t *testing.T) {
	projectDir, resolved, projectName := importTestProject(t)

	cases := []struct {
		name          string
		containers    []api.AgentInfo
		listErr       error
		wantProvision bool
		wantLine      string
		wantWarning   string
	}{
		{name: "running container is adopted", containers: []api.AgentInfo{localContainer("c-a1", "a1", "running", resolved)}, wantProvision: false, wantLine: "+ a1 (running, adopted)\n"},
		{name: "running container matched by project name label", containers: []api.AgentInfo{{ContainerID: "c-a1", Name: "a1", Phase: "running", Project: projectName, Labels: map[string]string{"scion.name": "a1"}}}, wantProvision: false, wantLine: "+ a1 (running, adopted)\n"},
		{name: "exited container is removed and the agent imported as stopped", containers: []api.AgentInfo{localContainer("c-a1", "a1", "stopped", resolved)}, wantProvision: true, wantLine: "+ a1 (imported as created (not started); its exited container was removed)\n"},
		{name: "no container is imported as created", wantProvision: true, wantLine: "+ a1 (imported as created (not started))\n"},
		{name: "running same-name agent in another project does not count", containers: []api.AgentInfo{
			{ContainerID: "c-x", Name: "a1", Phase: "running", ProjectPath: "/elsewhere/other/.scion", Project: projectName, Labels: map[string]string{"scion.name": "a1", "scion.project_path": "/elsewhere/other/.scion"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created (not started))\n"},
		{name: "running same-name agent with another project name does not count", containers: []api.AgentInfo{
			{ContainerID: "c-x", Name: "a1", Phase: "running", Project: "some-other-project", Labels: map[string]string{"scion.name": "a1"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created (not started))\n"},
		{name: "running container without project labels does not count", containers: []api.AgentInfo{
			{ContainerID: "c-x", Name: "a1", Phase: "running", Labels: map[string]string{"scion.name": "a1"}},
		}, wantProvision: true, wantLine: "+ a1 (imported as created (not started))\n"},
		{name: "runtime error does not start the agent", listErr: errors.New("daemon not reachable"), wantProvision: true, wantLine: "+ a1 (imported as created (not started))\n", wantWarning: "could not check which agents are running"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
				return tc.containers, tc.listErr
			}, nil, "a1")

			assert.Equal(t, 1, r.listCalls)
			assert.Equal(t, []map[string]string{{"scion.agent": "true"}}, r.filters)
			require.Len(t, r.reqs, 1)
			assert.Equal(t, "a1", r.reqs[0].Name)
			assert.Equal(t, tc.wantProvision, r.reqs[0].ProvisionOnly)
			assert.Contains(t, r.out, tc.wantLine)
			if tc.wantWarning != "" {
				assert.Contains(t, r.warn, tc.wantWarning)
				assert.Contains(t, r.warn, "daemon not reachable")
			} else {
				assert.Empty(t, r.warn)
			}
		})
	}
}

// TestExecuteSync_ImportRemovesExitedLeftoverFirst checks that the exited
// container of a suspended or stopped agent is removed before the agent is
// created on the hub, so the hub never sees its exit code (which would put
// the agent in error, where start and resume refuse it).
func TestExecuteSync_ImportRemovesExitedLeftoverFirst(t *testing.T) {
	projectDir, resolved, _ := importTestProject(t)
	r := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
		return []api.AgentInfo{localContainer("c-s1", "s1", "stopped", resolved)}, nil
	}, nil, "s1")

	assert.Equal(t, []string{"delete c-s1", "create s1"}, r.events, "removal comes before the create")
	require.Len(t, r.deleted, 1)
	assert.Equal(t, runtime.RunRef{ID: "c-s1", RunID: "run-c-s1"}, r.deleted[0])
	require.Len(t, r.reqs, 1)
	assert.True(t, r.reqs[0].ProvisionOnly)
	assert.Contains(t, r.out, "+ s1 (imported as created (not started); its exited container was removed)\n")
	assert.Empty(t, r.warn)
}

// TestExecuteSync_ImportNeverRemovesOtherContainers checks that the import
// removes nothing but this agent's exited (phase stopped) container in this
// project: not a running container, not a created, restarting or dead one
// (a concurrent start can leave a created container), not another
// project's container, and not a container whose project is known only by
// name.
func TestExecuteSync_ImportNeverRemovesOtherContainers(t *testing.T) {
	projectDir, resolved, projectName := importTestProject(t)
	for _, tc := range []struct {
		name       string
		containers []api.AgentInfo
		wantLine   string
		wantHint   bool
	}{
		{name: "running container", containers: []api.AgentInfo{localContainer("c-a1", "a1", "running", resolved)}, wantLine: "+ a1 (running, adopted)\n"},
		{name: "exited container of another project", containers: []api.AgentInfo{localContainer("c-x", "a1", "stopped", "/elsewhere/other/.scion")}, wantLine: "+ a1 (imported as created (not started))\n"},
		{name: "created (not exited) container, as a concurrent start leaves it", containers: []api.AgentInfo{localContainer("c-new", "a1", "created", resolved)},
			wantLine: "+ a1 (imported as created (not started))\n", wantHint: true},
		{name: "dead or restarting container (phase not stopped)", containers: []api.AgentInfo{localContainer("c-dead", "a1", "error", resolved)},
			wantLine: "+ a1 (imported as created (not started))\n", wantHint: true},
		{name: "exited container known only by project name", containers: []api.AgentInfo{
			{ContainerID: "c-n", Name: "a1", Phase: "stopped", Project: projectName, Labels: map[string]string{"scion.name": "a1"}},
		}, wantLine: "+ a1 (imported as created (not started))\n", wantHint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runImport(t, projectDir, func() ([]api.AgentInfo, error) { return tc.containers, nil }, nil, "a1")
			assert.Empty(t, r.deleted, "nothing is removed")
			assert.Contains(t, r.out, tc.wantLine)
			if tc.wantHint {
				assert.Contains(t, r.warn, "run 'scion stop a1' before 'scion start a1'")
			} else {
				assert.Empty(t, r.warn)
			}
		})
	}
}

// TestExecuteSync_ImportRemovalFailureWarns checks that a failed removal is
// a warning with the stop-then-start recovery, and the agent is still
// provisioned without starting.
func TestExecuteSync_ImportRemovalFailureWarns(t *testing.T) {
	projectDir, resolved, _ := importTestProject(t)
	r := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
		return []api.AgentInfo{localContainer("c-s1", "s1", "stopped", resolved)}, nil
	}, errors.New("permission denied"), "s1")

	require.Len(t, r.reqs, 1)
	assert.True(t, r.reqs[0].ProvisionOnly, "still provisioned without starting")
	assert.Contains(t, r.warn, "could not remove the exited container of agent 's1'")
	assert.Contains(t, r.warn, "permission denied")
	assert.Contains(t, r.warn, "run 'scion stop s1' before 'scion start s1'")
	assert.Contains(t, r.out, "+ s1 (imported as created (not started))\n")
}

// TestExecuteSync_ImportMixedAgents imports several agents in one sync and
// checks that the runtime is listed once and each request gets its own
// decision.
func TestExecuteSync_ImportMixedAgents(t *testing.T) {
	projectDir, resolved, _ := importTestProject(t)
	r := runImport(t, projectDir, func() ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			localContainer("c-run", "run", "running", resolved),
			localContainer("c-off", "off", "stopped", resolved),
			localContainer("c-idle", "idle", "running", "/elsewhere/.scion"),
		}, nil
	}, nil, "idle", "run", "off")

	assert.Equal(t, 1, r.listCalls, "the runtime is listed once for the whole import")
	require.Len(t, r.reqs, 3)
	assert.True(t, r.reqs[0].ProvisionOnly, "idle agent is provisioned only")
	assert.False(t, r.reqs[1].ProvisionOnly, "running agent keeps create and start")
	assert.True(t, r.reqs[2].ProvisionOnly, "stopped agent is provisioned only")
	assert.Equal(t, []string{"create idle", "create run", "delete c-off", "create off"}, r.events)
	assert.Equal(t, "+ idle (imported as created (not started))\n+ run (running, adopted)\n+ off (imported as created (not started); its exited container was removed)\nAgent synchronization complete.\n", r.out)
}

// TestExecuteSync_NoRegistrationsSkipsRuntime checks that a sync with
// nothing to register does not query the runtime.
func TestExecuteSync_NoRegistrationsSkipsRuntime(t *testing.T) {
	projectDir, _, _ := importTestProject(t)
	r := runImport(t, projectDir, func() ([]api.AgentInfo, error) { return nil, nil }, nil)
	assert.Equal(t, 0, r.listCalls)
}
