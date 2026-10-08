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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workspaceCheckDispatcher is a lifecycle dispatcher whose exec answers the
// pre-stop workspace check with execOutput, or execErr, or (block) waits for
// the exec context to end.
type workspaceCheckDispatcher struct {
	quotaLifecycleDispatcher
	mu         sync.Mutex
	execCalls  int
	execOutput string
	execErr    error
	block      bool
	// stopErr, when set, is returned by DispatchAgentStop.
	stopErr error
}

func (d *workspaceCheckDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return d.quotaLifecycleDispatcher.DispatchAgentStop(ctx, agent)
}

func (d *workspaceCheckDispatcher) DispatchAgentExec(ctx context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	d.mu.Lock()
	d.execCalls++
	out, err, block := d.execOutput, d.execErr, d.block
	d.mu.Unlock()
	if block {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	return out, 0, err
}

func (d *workspaceCheckDispatcher) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execCalls
}

const workAt23 = "scion-workspace-check commits=2 files=3\n"

func newWorkspaceCheckServer(t *testing.T, disp *workspaceCheckDispatcher) (*Server, store.Store, *store.RuntimeBroker, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	broker, project := newQuotaTestBrokerAndProject(t, s, "wscheck")
	return srv, s, broker, project
}

// newWorkspaceAgent creates an agent of runtime rt with workspace placement
// placement (empty: none reported) in phase.
func newWorkspaceAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name, rt, placement string, phase state.Phase) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Runtime:         rt,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	if placement != "" {
		require.NoError(t, s.SetAgentWorkspacePlacement(context.Background(), a.ID, placement))
	}
	return a
}

func lifecycleWarnings(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Warnings
}

func workspaceAnnotation(t *testing.T, s store.Store, id string) string {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.Annotations[workspaceAtStopAnnotation]
}

const (
	stopWarning23  = "Workspace is ephemeral and will be re-cloned on next start; 2 unpushed commits and 3 changed files will be lost. Push first to keep them."
	startWarning23 = "Workspace is ephemeral and was re-cloned; 2 unpushed commits and 3 changed files from the previous run were lost."
)

func TestEphemeralWorkspace_StopWarnsWithCommitsAndFiles(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-work", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{stopWarning23}, lifecycleWarnings(t, rec.Body.Bytes()))
	assert.Equal(t, 1, disp.calls())
	assert.JSONEq(t, `{"commits":2,"files":3}`, workspaceAnnotation(t, s, a.ID))
}

func TestEphemeralWorkspace_StopCleanNoWarning(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: "scion-workspace-check commits=0 files=0\n"}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-clean", "k8s", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
	assert.Equal(t, 1, disp.calls())
	assert.JSONEq(t, `{}`, workspaceAnnotation(t, s, a.ID))
}

// Docker, Kubernetes on the NFS export, and an unknown placement are left
// exactly as before: no exec, no warning, no record, on stop or start.
func TestEphemeralWorkspace_NotEphemeralUntouched(t *testing.T) {
	cases := []struct{ name, rt, placement string }{
		{"ws-docker", "docker", api.WorkspacePlacementLocal},
		{"ws-nfs", "kubernetes", api.WorkspacePlacementExport},
		{"ws-unknown", "kubernetes", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &workspaceCheckDispatcher{execOutput: workAt23}
			srv, s, broker, project := newWorkspaceCheckServer(t, disp)
			a := newWorkspaceAgent(t, s, broker, project, tc.name, tc.rt, tc.placement, state.PhaseRunning)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
			assert.Zero(t, disp.calls(), "no exec for a non-ephemeral workspace")
			assert.Empty(t, workspaceAnnotation(t, s, a.ID))

			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
		})
	}
}

// A failing or hanging check never blocks the stop: it proceeds without a
// warning, within the check's time bound, and records the result as
// unchecked.
func TestEphemeralWorkspace_CheckFailureDoesNotBlockStop(t *testing.T) {
	old := workspaceCheckTimeout
	// 1.5s for the exec, 300ms reserved for the record write.
	workspaceCheckTimeout = 1800 * time.Millisecond
	t.Cleanup(func() { workspaceCheckTimeout = old })

	cases := []struct {
		name string
		disp *workspaceCheckDispatcher
	}{
		{"ws-exec-error", &workspaceCheckDispatcher{execErr: errors.New("container not found")}},
		{"ws-exec-timeout", &workspaceCheckDispatcher{block: true}},
		{"ws-exec-garbage", &workspaceCheckDispatcher{execOutput: "fatal: not a git repository"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, broker, project := newWorkspaceCheckServer(t, tc.disp)
			a := newWorkspaceAgent(t, s, broker, project, tc.name, "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

			start := time.Now()
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Less(t, time.Since(start), 4*time.Second, "the check and its record write stay within the check budget")
			assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
			assert.EqualValues(t, 1, tc.disp.stopCount.Load(), "the stop is dispatched")
			got, err := s.GetAgent(context.Background(), a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
			assert.JSONEq(t, `{"unchecked":true}`, got.Annotations[workspaceAtStopAnnotation])
		})
	}
}

// The check is skipped for an agent that is not running.
func TestEphemeralWorkspace_StopNotRunningSkipsCheck(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-errored", "kubernetes", api.WorkspacePlacementLocal, state.PhaseError)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Zero(t, disp.calls())
	assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
}

// A start after a stop repeats the recorded result as lost, then clears the
// record, so the next start without a recorded stop gets the generic notice.
func TestEphemeralWorkspace_ResumeShowsRecordedResult(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-resume", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{startWarning23}, lifecycleWarnings(t, rec.Body.Bytes()))
	assert.Empty(t, workspaceAnnotation(t, s, a.ID), "the record is cleared by the start")

	// The pod goes away without a hub stop (no record): generic notice.
	require.NoError(t, s.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{ephemeralWorkspaceRecloneNotice}, lifecycleWarnings(t, rec.Body.Bytes()))
}

// A clean recorded stop gives no start warning.
func TestEphemeralWorkspace_ResumeAfterCleanStopNoWarning(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: "scion-workspace-check commits=0 files=0"}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-resume-clean", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
}

// Suspend runs the same check with the stop text, and the resume after it
// uses the recorded result.
func TestEphemeralWorkspace_SuspendWarnsAndResumeReports(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-suspend", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{stopWarning23}, lifecycleWarnings(t, rec.Body.Bytes()))
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{startWarning23}, lifecycleWarnings(t, rec.Body.Bytes()))
}

// A restart reports the result once, from its start leg.
func TestEphemeralWorkspace_RestartWarnsOnce(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-restart", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{startWarning23}, lifecycleWarnings(t, rec.Body.Bytes()))
	assert.Equal(t, 1, disp.calls())
}

func TestParseWorkspaceCheckOutput(t *testing.T) {
	cases := []struct {
		in             string
		commits, files int
		ok             bool
	}{
		{"scion-workspace-check commits=2 files=3\n", 2, 3, true},
		{"noise\nscion-workspace-check commits=0 files=12\n", 0, 12, true},
		{"scion-workspace-check commits=   7 files=1", 0, 0, false},
		{"scion-workspace-check commits=x files=1", 0, 0, false},
		{"scion-workspace-check commits=-1 files=1", 0, 0, false},
		{"", 0, 0, false},
		{strings.Repeat("x", workspaceCheckMaxOutput) + "\nscion-workspace-check commits=1 files=1", 0, 0, false},
	}
	for _, tc := range cases {
		c, f, ok := parseWorkspaceCheckOutput(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.commits, c, tc.in)
		assert.Equal(t, tc.files, f, tc.in)
	}
}

func TestEphemeralWorkspaceWarningText(t *testing.T) {
	assert.Equal(t, "Workspace is ephemeral and will be re-cloned on next start; 1 unpushed commit will be lost. Push first to keep it.",
		ephemeralWorkspaceStopWarning(ephemeralWorkspaceRecord{Commits: 1}))
	assert.Equal(t, "Workspace is ephemeral and was re-cloned; 1 changed file from the previous run was lost.",
		ephemeralWorkspaceStartWarning(&ephemeralWorkspaceRecord{Files: 1}))
	assert.Empty(t, ephemeralWorkspaceStopWarning(ephemeralWorkspaceRecord{Unchecked: true}))
	assert.Equal(t, ephemeralWorkspaceRecloneNotice, ephemeralWorkspaceStartWarning(nil))
	assert.Equal(t, ephemeralWorkspaceRecloneNotice, ephemeralWorkspaceStartWarning(&ephemeralWorkspaceRecord{Unchecked: true}))
}

// A lifecycle start of a running agent keeps the running pod (the broker
// returns the existing run), so there is no re-clone notice.
func TestEphemeralWorkspace_StartOfRunningAgentNoNotice(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-start-running", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, lifecycleWarnings(t, rec.Body.Bytes()))
	assert.Zero(t, disp.calls())
}

// When the stop (or suspend) dispatch fails, the agent may still be
// running: the record the check wrote is removed, so a later start does not
// report it.
func TestEphemeralWorkspace_FailedStopClearsRecord(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			disp := &workspaceCheckDispatcher{execOutput: workAt23, stopErr: errors.New("broker failed the stop")}
			srv, s, broker, project := newWorkspaceCheckServer(t, disp)
			a := newWorkspaceAgent(t, s, broker, project, "ws-failed-"+action, "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)
			require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, 1, disp.calls(), "the check ran before the failed dispatch")
			assert.Empty(t, workspaceAnnotation(t, s, a.ID), "a failed stop leaves no record")
		})
	}
}

// scion start and scion resume restart an existing agent through the create
// call (handleExistingAgent): a stopped agent with resume restarts in place,
// a suspended one is resumed. The create response carries the start warning
// too.
func TestEphemeralWorkspace_CreateOnExistingShowsRecordedResult(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "start"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			disp := &workspaceCheckDispatcher{execOutput: workAt23}
			srv, s, broker, project := newWorkspaceCheckServer(t, disp)
			ctx := context.Background()
			owner := tid("ws-create-owner")
			ensureStandingRoot(t, s, project.ID, owner)
			a := &store.Agent{
				ID:              tid("agent-ws-create-" + name),
				Slug:            "ws-create-" + name,
				Name:            "ws-create-" + name,
				ProjectID:       project.ID,
				OwnerID:         owner,
				RuntimeBrokerID: broker.ID,
				Runtime:         "kubernetes",
				Phase:           string(state.PhaseRunning),
			}
			require.NoError(t, s.CreateAgent(ctx, a))
			require.NoError(t, s.SetAgentWorkspacePlacement(ctx, a.ID, api.WorkspacePlacementLocal))

			action := "stop"
			if resume {
				action = "suspend"
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: a.Name, ProjectID: project.ID, Task: "x", Resume: true,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)
			assert.Equal(t, a.ID, resp.Agent.ID, "the existing agent was started, not recreated")
			assert.Contains(t, resp.Warnings, startWarning23)
			assert.Empty(t, workspaceAnnotation(t, s, a.ID))
		})
	}
}
