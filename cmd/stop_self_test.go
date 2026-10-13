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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopSelfHubAgent is an agent listed by newStopSelfHub.
type stopSelfHubAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// stopSelfEvents records the stop and delete requests a mock Hub receives,
// as "stop:<agent>" and "delete:<agent>".
type stopSelfEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *stopSelfEvents) add(ev string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *stopSelfEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// newStopSelfHub serves a project whose agent list is agents, all running.
// A stop answers 200 (applied) and a delete 204 (confirmed); both are
// recorded.
func newStopSelfHub(t *testing.T, projectID string, agents []stopSelfHubAgent) (*httptest.Server, *stopSelfEvents) {
	t.Helper()
	events := &stopSelfEvents{}
	prefix := "/api/v1/projects/" + projectID + "/agents"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == prefix:
			list := make([]map[string]interface{}, 0, len(agents))
			for _, a := range agents {
				list = append(list, map[string]interface{}{"id": a.ID, "name": a.Name, "phase": "running"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": list, "totalCount": len(list)})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix+"/") && strings.HasSuffix(r.URL.Path, "/stop"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"/"), "/stop")
			events.add("stop:" + name)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": name, "name": name, "phase": "stopped"})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/"):
			events.add("delete:" + strings.TrimPrefix(r.URL.Path, prefix+"/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, events
}

// setupStopSelf saves and resets the stop and project state for one test,
// sets the current agent's identity (empty values clear it), and returns
// the project directory. HOME and the project are temp dirs.
func setupStopSelf(t *testing.T, name, id, ownProjectID string) string {
	t.Helper()
	orig := saveDeleteTestState()
	oldRm, oldAll, oldSelf, oldFormat := stopRm, stopAll, stopSelf, outputFormat
	t.Cleanup(func() {
		orig.restore()
		stopRm, stopAll, stopSelf, outputFormat = oldRm, oldAll, oldSelf, oldFormat
	})

	setCurrentAgentEnv(t, name, name, id, ownProjectID)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	noHub = false
	autoConfirm = true
	stopRm, stopAll, stopSelf, outputFormat = false, false, false, ""

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755))
	projectPath = projectDir
	return projectDir
}

// setupStopSelfViaHub is setupStopSelf for a run of the stop command
// against a mock Hub for project selfProjectID listing agents.
func setupStopSelfViaHub(t *testing.T, name, id, ownProjectID string, agents []stopSelfHubAgent) (string, *stopSelfEvents) {
	t.Helper()
	projectDir := setupStopSelf(t, name, id, ownProjectID)
	// projectPath stands in for an explicit --project flag, whose own
	// project ID wins over SCION_PROJECT_ID (ptone/scion#3123).
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project-id"), []byte(selfProjectID+"\n"), 0o644))
	server, events := newStopSelfHub(t, selfProjectID, agents)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_HUB_URL", server.URL)
	return projectDir, events
}

// runStop runs the stop command's RunE and returns its stderr and error.
func runStop(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = stopCmd.RunE(stopCmd, append([]string(nil), args...))
	})
	return stderr, runErr
}

// stopSelfAllAgents is the agent list of the --all tests: the current agent
// and two others.
var stopSelfAllAgents = []stopSelfHubAgent{
	{ID: "id-other", Name: "other-agent"},
	{ID: selfAgentID, Name: selfAgentName},
	{ID: "id-third", Name: "third-agent"},
}

// runStopAllViaHub runs stop --all (--rm if rm, --self if self) against a
// mock Hub for project selfProjectID listing agents, inside the agent given
// by name and id (empty values: not inside an agent). It returns stderr,
// the recorded requests and the error.
func runStopAllViaHub(t *testing.T, name, id string, agents []stopSelfHubAgent, rm, self bool) (string, []string, error) {
	t.Helper()
	setupStopSelf(t, name, id, selfProjectID)
	server, events := newStopSelfHub(t, selfProjectID, agents)
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: selfProjectID}
	stopAll, stopRm, stopSelf = true, rm, self

	var runErr error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { runErr = stopAllAgentsViaHub(hubCtx) })
	})
	return stderr, events.list(), runErr
}

// stop --rm naming the current agent without --self is refused before the
// stop: the Hub receives neither a stop nor a delete.
func TestStopCmd_RmCurrentAgentWithoutSelfRefused(t *testing.T) {
	_, events := setupStopSelfViaHub(t, selfAgentName, selfAgentID, selfProjectID, nil)
	stopRm = true

	_, err := runStop(t, selfAgentName)
	require.Error(t, err, "stop --rm of the current agent without --self must be refused")
	assert.Contains(t, err.Error(), "is the current agent")
	assert.Contains(t, err.Error(), "--self is required")
	assert.Empty(t, events.list(), "a refused stop --rm must not stop or delete anything")
}

// With --self, stop --rm stops and removes the current agent as usual.
func TestStopCmd_RmCurrentAgentWithSelfProceeds(t *testing.T) {
	_, events := setupStopSelfViaHub(t, selfAgentName, selfAgentID, selfProjectID, nil)
	stopRm, stopSelf = true, true

	_, err := runStop(t, selfAgentName)
	require.NoError(t, err, "--self must allow stop --rm of the current agent")
	assert.Equal(t, []string{"stop:" + selfAgentName, "delete:" + selfAgentName}, events.list())
}

// stop --rm of another agent from inside an agent is unaffected.
func TestStopCmd_RmOtherAgentFromInsideAgentProceeds(t *testing.T) {
	_, events := setupStopSelfViaHub(t, selfAgentName, selfAgentID, selfProjectID, nil)
	stopRm = true

	_, err := runStop(t, "other-agent")
	require.NoError(t, err, "stop --rm of another agent must not be refused")
	assert.Equal(t, []string{"stop:other-agent", "delete:other-agent"}, events.list())
}

// A plain stop (no --rm) of the current agent needs no --self.
func TestStopCmd_PlainStopCurrentAgentProceeds(t *testing.T) {
	_, events := setupStopSelfViaHub(t, selfAgentName, selfAgentID, selfProjectID, nil)

	_, err := runStop(t, selfAgentName)
	require.NoError(t, err, "a stop without --rm must not be refused")
	assert.Equal(t, []string{"stop:" + selfAgentName}, events.list())
}

// Outside an agent, stop --rm behaves as before.
func TestStopCmd_RmOutsideAgentProceeds(t *testing.T) {
	_, events := setupStopSelfViaHub(t, "", "", "", nil)
	stopRm = true

	_, err := runStop(t, selfAgentName)
	require.NoError(t, err, "outside an agent stop --rm must not be refused")
	assert.Equal(t, []string{"stop:" + selfAgentName, "delete:" + selfAgentName}, events.list())
}

// Without a Hub, stop --rm of the current agent is refused (by name, fail
// closed) before the local stop.
func TestStopCmd_LocalRmCurrentAgentWithoutSelfRefused(t *testing.T) {
	setupStopSelf(t, selfAgentName, selfAgentID, selfProjectID)
	noHub = true
	stopRm = true

	_, err := runStop(t, selfAgentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--self is required",
		"the local stop --rm must be refused by the guard, not fail later in the runtime")
}

// stop --all --rm skips the current agent, notes it, and stops and removes
// the others.
func TestStopAllViaHub_RmSkipsCurrentAgent(t *testing.T) {
	stderr, events, err := runStopAllViaHub(t, selfAgentName, selfAgentID, stopSelfAllAgents, true, false)
	require.NoError(t, err)
	assert.NotContains(t, events, "stop:"+selfAgentName, "stop --all --rm must not stop the current agent")
	assert.NotContains(t, events, "delete:"+selfAgentName, "stop --all --rm must not remove the current agent")
	assert.ElementsMatch(t, []string{"stop:other-agent", "delete:other-agent", "stop:third-agent", "delete:third-agent"}, events)
	assert.Contains(t, stderr, "Skipping agent 'self-agent': it is the current agent")
	assert.Contains(t, stderr, "add --self")
}

// The current agent is skipped when it matches by agent ID only.
func TestStopAllViaHub_RmSkipsCurrentAgentByID(t *testing.T) {
	agents := []stopSelfHubAgent{{ID: "id-other", Name: "other-agent"}, {ID: selfAgentID, Name: "renamed-agent"}}
	_, events, err := runStopAllViaHub(t, selfAgentName, selfAgentID, agents, true, false)
	require.NoError(t, err)
	assert.NotContains(t, events, "stop:renamed-agent", "the agent with the current agent's ID must be skipped")
	assert.ElementsMatch(t, []string{"stop:other-agent", "delete:other-agent"}, events)
}

// stop --all --rm --self includes the current agent.
func TestStopAllViaHub_RmWithSelfIncludesCurrentAgent(t *testing.T) {
	stderr, events, err := runStopAllViaHub(t, selfAgentName, selfAgentID, stopSelfAllAgents, true, true)
	require.NoError(t, err)
	assert.Contains(t, events, "stop:"+selfAgentName, "--self must include the current agent")
	assert.Contains(t, events, "delete:"+selfAgentName, "--self must include the current agent")
	assert.Len(t, events, 6)
	assert.NotContains(t, stderr, "Skipping agent")
}

// A plain stop --all (no --rm) still stops the current agent.
func TestStopAllViaHub_PlainStopIncludesCurrentAgent(t *testing.T) {
	_, events, err := runStopAllViaHub(t, selfAgentName, selfAgentID, stopSelfAllAgents, false, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"stop:other-agent", "stop:" + selfAgentName, "stop:third-agent"}, events,
		"stop --all without --rm is unchanged")
}

// Outside an agent, stop --all --rm stops and removes every agent.
func TestStopAllViaHub_RmOutsideAgentIncludesAll(t *testing.T) {
	stderr, events, err := runStopAllViaHub(t, "", "", stopSelfAllAgents, true, false)
	require.NoError(t, err)
	assert.Len(t, events, 6, "outside an agent no agent is skipped")
	assert.Contains(t, events, "delete:"+selfAgentName)
	assert.NotContains(t, stderr, "Skipping agent")
}

// The local stop --all --rm uses the same helper with an unknown project
// (no Hub): the current agent is skipped by name.
func TestSkipCurrentAgentForStopAllRm(t *testing.T) {
	setupStopSelf(t, selfAgentName, selfAgentID, selfProjectID)
	names := []string{"other-agent", selfAgentName}
	identity := func(n string) []string { return []string{n} }

	stopRm = true
	var got []string
	stderr := captureStderr(t, func() { got = skipCurrentAgentForStopAllRm(names, "", identity) })
	assert.Equal(t, []string{"other-agent"}, got, "with --rm and an unknown project the current agent is skipped by name")
	assert.Contains(t, stderr, "Skipping agent 'self-agent'")

	stopSelf = true
	assert.Equal(t, names, skipCurrentAgentForStopAllRm(names, "", identity), "--self keeps the current agent")

	stopRm, stopSelf = false, false
	assert.Equal(t, names, skipCurrentAgentForStopAllRm(names, "", identity), "without --rm nothing is skipped")
}
