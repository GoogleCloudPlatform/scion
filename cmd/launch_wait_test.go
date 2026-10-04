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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int { return &v }

func activeLaunch(step string, remaining int, deadline *time.Time) *hubclient.AgentLaunch {
	return &hubclient.AgentLaunch{
		ID: "launch-1", State: "active", Active: true, Kind: "create", Step: step,
		RemainingSeconds: intPtr(remaining), Deadline: deadline,
	}
}

func TestLaunchWaitBudget(t *testing.T) {
	withLaunch := &hubclient.Agent{Launch: activeLaunch("", 270, nil)}
	tests := []struct {
		name     string
		explicit time.Duration
		agent    *hubclient.Agent
		want     time.Duration
	}{
		{"derived from remainingSeconds", 0, withLaunch, 300 * time.Second},
		{"remainingSeconds 0 waits 30s", 0, &hubclient.Agent{Launch: activeLaunch("", 0, nil)}, 30 * time.Second},
		{"negative remainingSeconds clamps", 0, &hubclient.Agent{Launch: activeLaunch("", -5, nil)}, 30 * time.Second},
		{"no launch falls back to 5m", 0, &hubclient.Agent{}, 5 * time.Minute},
		{"launch without remainingSeconds falls back", 0, &hubclient.Agent{Launch: &hubclient.AgentLaunch{State: "ended"}}, 5 * time.Minute},
		{"nil agent falls back", 0, nil, 5 * time.Minute},
		{"explicit wins over derived", 45 * time.Second, withLaunch, 45 * time.Second},
		{"explicit wins over fallback", 10 * time.Minute, nil, 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, launchWaitBudget(tt.explicit, tt.agent))
		})
	}
}

// agentSequence returns a Get func that yields each result in turn and then
// repeats the last one.
type agentSequence struct {
	mu      sync.Mutex
	results []func() (*hubclient.Agent, error)
	calls   int
}

func (s *agentSequence) get(ctx context.Context) (*hubclient.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.calls++
	return s.results[i]()
}

func agentResult(a *hubclient.Agent) func() (*hubclient.Agent, error) {
	return func() (*hubclient.Agent, error) { cp := *a; return &cp, nil }
}

func errResult(err error) func() (*hubclient.Agent, error) {
	return func() (*hubclient.Agent, error) { return nil, err }
}

func TestWaitForAgentLaunch_RunningPrintsProgress(t *testing.T) {
	shortenLaunchWaitTimings(t)
	seq := &agentSequence{results: []func() (*hubclient.Agent, error){
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)}),
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)}),
		errResult(errors.New("connection reset")), // transient: retried
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("image_pull", 2, nil)}),
		agentResult(&hubclient.Agent{ID: "id-1", Phase: "running", Activity: "working"}),
	}}
	var out bytes.Buffer
	a, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
		AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second, Progress: &out,
	})
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "id-1", a.ID)
	assert.Equal(t, "  a1: provisioning (pod_create)\n  a1: provisioning (image_pull)\n  a1: running\n", out.String())
	assert.Equal(t, 5, seq.calls)
}

func TestWaitForAgentLaunch_SilentWithoutProgressWriter(t *testing.T) {
	shortenLaunchWaitTimings(t)
	seq := &agentSequence{results: []func() (*hubclient.Agent, error){
		agentResult(&hubclient.Agent{Phase: "running"}),
	}}
	out := captureStderr(t, func() {
		_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond})
		require.NoError(t, err)
	})
	assert.Empty(t, out)
}

func TestWaitForAgentLaunch_TerminalStates(t *testing.T) {
	shortenLaunchWaitTimings(t)
	tests := []struct {
		name           string
		agent          *hubclient.Agent
		wantIncomplete bool
		wantContains   []string
		wantMissing    []string
	}{
		{
			name: "create failed with launch error",
			agent: &hubclient.Agent{
				Phase: "error", Template: "claude", Message: "image pull failed for example.com/img:bad",
				AppliedConfig: &hubclient.AgentConfig{Task: "fix the tests"},
				Launch:        &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "create", Error: "image_pull_failed", EndReason: "failed"},
			},
			wantIncomplete: true,
			wantContains: []string{
				"agent 'a1' create did not complete (image_pull_failed): image pull failed for example.com/img:bad",
				"Template: claude", "Task: fix the tests",
				"scion delete a1", "scion start a1", "force=true or purged",
			},
			wantMissing: []string{"scion logs"},
		},
		{
			name: "create stopped",
			agent: &hubclient.Agent{
				Phase:  "stopped",
				Launch: &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "create", Error: "launch_stopped", EndReason: "stopped"},
			},
			wantIncomplete: true,
			wantContains:   []string{"create did not complete (launch_stopped)", "scion delete a1"},
			wantMissing:    []string{"Template:", "Task:"},
		},
		{
			name: "launch timed out on the Hub",
			agent: &hubclient.Agent{
				Phase: "error", Message: "launch timed out during image_pull",
				Launch: &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "create", Error: "launch_timeout", EndReason: "timed_out"},
			},
			wantIncomplete: true,
			wantContains:   []string{"(launch_timeout): launch timed out during image_pull", "scion delete a1"},
		},
		{
			name: "start launch error is not an incomplete create",
			agent: &hubclient.Agent{
				Phase: "error", Template: "claude",
				Launch: &hubclient.AgentLaunch{ID: "l2", State: "ended", Kind: "start", Error: "image_pull_failed", EndReason: "failed"},
			},
			wantContains: []string{"agent 'a1' did not start (image_pull_failed)", "scion logs a1"},
			wantMissing:  []string{"scion delete", "create did not complete", "Template:"},
		},
		{
			name:         "error without launch error",
			agent:        &hubclient.Agent{Phase: "error", ContainerStatus: "Exited (1)"},
			wantContains: []string{"agent 'a1' failed to start (phase: error, container: Exited (1))", "scion logs a1"},
			wantMissing:  []string{"scion delete"},
		},
		{
			name:         "stopped without launch",
			agent:        &hubclient.Agent{Phase: "stopped"},
			wantContains: []string{"failed to start (phase: stopped)", "scion logs a1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)}),
				agentResult(tt.agent),
			}}
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
			})
			var failed *launchFailedError
			require.ErrorAs(t, err, &failed)
			assert.Equal(t, tt.wantIncomplete, failed.Incomplete)
			for _, s := range tt.wantContains {
				assert.Contains(t, err.Error(), s)
			}
			for _, s := range tt.wantMissing {
				assert.NotContains(t, err.Error(), s)
			}
			var ec exitCoder
			assert.False(t, errors.As(err, &ec), "a failed launch exits 1")
		})
	}
}

func TestWaitForAgentLaunch_Timeout(t *testing.T) {
	shortenLaunchWaitTimings(t)
	deadline := time.Date(2026, 10, 2, 12, 5, 0, 0, time.UTC)
	tests := []struct {
		name   string
		launch *hubclient.AgentLaunch
		want   string
	}{
		{"with deadline", activeLaunch("scheduling", 2, &deadline),
			"agent 'a1' is still launching (deadline 2026-10-02T12:05:00Z); re-run scion start a1 to keep waiting"},
		{"without deadline (older Hub)", nil,
			"agent 'a1' is still launching; re-run scion start a1 to keep waiting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				agentResult(&hubclient.Agent{Phase: "provisioning", Launch: tt.launch}),
			}}
			start := time.Now()
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: 5 * time.Millisecond, Timeout: 60 * time.Millisecond,
			})
			var timeout *launchWaitTimeoutError
			require.ErrorAs(t, err, &timeout)
			assert.Equal(t, tt.want, err.Error())
			assert.Less(t, time.Since(start), 2*time.Second)
			var ec exitCoder
			assert.False(t, errors.As(err, &ec), "a wait timeout exits 1")
		})
	}
}

func TestWaitForAgentLaunch_Interrupted(t *testing.T) {
	shortenLaunchWaitTimings(t)
	deadline := time.Date(2026, 10, 2, 12, 5, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	get := func(context.Context) (*hubclient.Agent, error) {
		calls++
		if calls == 2 {
			cancel() // the user presses Ctrl-C during the wait
		}
		return &hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("pod_create", 2, &deadline)}, nil
	}
	_, err := waitForAgentLaunch(ctx, launchWaitOptions{
		AgentName: "a1", Get: get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
	})
	var interrupted *launchWaitInterruptedError
	require.ErrorAs(t, err, &interrupted)
	assert.Contains(t, err.Error(), "stopped waiting; agent 'a1' is still launching (deadline 2026-10-02T12:05:00Z)")
	var ec exitCoder
	require.True(t, errors.As(err, &ec))
	assert.Equal(t, 130, ec.ExitCode())
}

func TestWaitForAgentLaunch_DeletedWhileWaiting(t *testing.T) {
	shortenLaunchWaitTimings(t)
	seq := &agentSequence{results: []func() (*hubclient.Agent, error){
		agentResult(&hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("", 2, nil)}),
		errResult(&apiclient.APIError{StatusCode: http.StatusNotFound, Code: "not_found", Message: "agent not found"}),
	}}
	_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
		AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent 'a1' no longer exists")
}

// p1b3IncompleteCreateMessage is the Hub's agent_create_incomplete message
// (pkg/hub/launch_guard.go incompleteCreateMessage).
const p1b3IncompleteCreateMessage = "agent a1 cannot be started: its create did not complete (image_pull_failed); " +
	"delete it and create it again; if soft-delete retention is enabled, the name stays reserved until the agent is " +
	"deleted with force=true or purged"

func TestIncompleteCreateError(t *testing.T) {
	apiErr := &apiclient.APIError{
		StatusCode: http.StatusConflict, Code: "agent_create_incomplete",
		Message: p1b3IncompleteCreateMessage,
		Details: map[string]interface{}{"template": "claude", "task": "fix the tests"},
	}
	got, ok := asIncompleteCreate(apiErr)
	require.True(t, ok)
	err := incompleteCreateError("a1", got)
	assert.True(t, err.Incomplete)
	msg := err.Error()
	assert.True(t, strings.HasPrefix(msg, apiErr.Message))
	assert.Contains(t, msg, "Template: claude")
	assert.Contains(t, msg, "Task: fix the tests")
	assert.True(t, err.HubOwnsHint)
	assert.Equal(t, 1, strings.Count(msg, "create it again"), "the recreate instruction is shown once: %q", msg)
	assert.Equal(t, 1, strings.Count(msg, "force=true or purged"), msg)
	assert.NotContains(t, msg, "Delete the agent and create it again", "the CLI hint is not added to the Hub's")

	// Without a Hub message, the CLI states the instruction itself.
	err = incompleteCreateError("a1", &apiclient.APIError{Code: "agent_create_incomplete"})
	assert.False(t, err.HubOwnsHint)
	assert.Equal(t, 1, strings.Count(err.Error(), "create it again"))
	assert.Contains(t, err.Error(), "scion delete a1")
	assert.Contains(t, err.Error(), "force=true or purged")

	_, ok = asIncompleteCreate(&apiclient.APIError{StatusCode: http.StatusConflict, Code: "conflict"})
	assert.False(t, ok)
}

// --- startAgentViaHub against a mock Hub ---

// launchMockHub is a minimal Hub for startAgentViaHub: the suspend-check
// GET, the create POST, and the GETs that follow the create.
type launchMockHub struct {
	t            *testing.T
	createStatus int
	createBody   interface{}
	afterCreate  []interface{} // GET responses after the create; the last repeats

	mu          sync.Mutex
	created     bool
	createReqs  []map[string]interface{}
	getsAfterCR int
}

func (h *launchMockHub) serve(projectID, agentName string) *httptest.Server {
	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	agentsPath := "/api/v1/projects/" + projectID + "/agents"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == agentsPath:
			body, _ := io.ReadAll(r.Body)
			var m map[string]interface{}
			require.NoError(h.t, json.Unmarshal(body, &m))
			h.createReqs = append(h.createReqs, m)
			h.created = true
			w.WriteHeader(h.createStatus)
			_ = json.NewEncoder(w).Encode(h.createBody)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/"+mockAttachBrokerID:
			_ = json.NewEncoder(w).Encode(mockAttachBroker(""))
		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			if !h.created {
				w.WriteHeader(http.StatusNotFound) // suspend check: no agent yet
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"code": "not_found", "message": "not found"}})
				return
			}
			i := h.getsAfterCR
			h.getsAfterCR++
			if len(h.afterCreate) == 0 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if i >= len(h.afterCreate) {
				i = len(h.afterCreate) - 1
			}
			_ = json.NewEncoder(w).Encode(h.afterCreate[i])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	h.t.Cleanup(srv.Close)
	return srv
}

// shortenLaunchWaitTimings makes polling immediate and the derived wait
// budgets short: slack 20ms, fallback 5s.
func shortenLaunchWaitTimings(t *testing.T) {
	t.Helper()
	origPoll, origSlack, origFallback := launchPollInterval, launchWaitSlack, launchWaitFallback
	t.Cleanup(func() {
		launchPollInterval, launchWaitSlack, launchWaitFallback = origPoll, origSlack, origFallback
	})
	launchPollInterval = time.Millisecond
	launchWaitSlack = 20 * time.Millisecond
	launchWaitFallback = 5 * time.Second
}

func setupLaunchStartTest(t *testing.T, hub *launchMockHub) *HubContext {
	t.Helper()
	restore := saveAttachTestState()
	origNoWait, origWait, origFormat := startNoWait, startWaitTimeout, outputFormat
	t.Cleanup(func() {
		restore()
		startNoWait, startWaitTimeout, outputFormat = origNoWait, origWait, origFormat
	})
	shortenLaunchWaitTimings(t)
	attach, templateName, labelFlags, runtimeBrokerID = false, "", nil, ""
	harnessConfigFlag, harnessAuthFlag = "", ""
	// A short explicit wait makes an unintended wait fail fast instead of
	// running to the default budget. Tests of the derived budget set it to 0.
	startNoWait, startWaitTimeout, outputFormat = false, 3*time.Second, ""

	srv := hub.serve(launchTestProjectID, "a1")
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: launchTestProjectID}
}

const launchTestProjectID = "proj-launch"

func TestStartAgentViaHub_SendsOptInAndOldHubSyncAnswerUnchanged(t *testing.T) {
	// An old Hub (or one with async launch off) ignores the opt-in and
	// answers synchronously with a running agent and no launch object.
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Name: "a1", Phase: "running"},
	}}
	hubCtx := setupLaunchStartTest(t, hub)

	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "do it", false, nil) })
	})
	require.NoError(t, err)
	require.Len(t, hub.createReqs, 1)
	assert.Equal(t, true, hub.createReqs[0]["acceptAsyncLaunch"], "the create request opts in")
	assert.Equal(t, 0, hub.getsAfterCR, "a synchronous answer is not followed by polling")
	assert.Contains(t, stderr, "Agent 'a1' started via Hub.")
	assert.Contains(t, stderr, "Phase: running")
	assert.NotContains(t, stderr, "Waiting for agent")
}

func TestStartAgentViaHub_EndedLaunchIsSynchronous(t *testing.T) {
	// A new Hub with an old broker ends the launch as not_launched and
	// answers synchronously: no wait.
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running",
			Launch: &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "create", EndReason: "not_launched"}},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	assert.Equal(t, 0, hub.getsAfterCR)
}

func asyncCreateResponse(warnings ...string) hubclient.CreateAgentResponse {
	return hubclient.CreateAgentResponse{
		Agent:    &hubclient.Agent{ID: "id-1", Slug: "a1", Name: "a1", Phase: "provisioning", Launch: activeLaunch("", 2, nil)},
		Warnings: warnings,
	}
}

func TestStartAgentViaHub_AsyncWaitsUntilRunning(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse(),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)},
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running", Activity: "working"},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	assert.Equal(t, 2, hub.getsAfterCR)
	assert.Contains(t, stderr, "Waiting for agent 'a1' to start...")
	assert.Contains(t, stderr, "  a1: provisioning (pod_create)")
	assert.Contains(t, stderr, "Agent 'a1' started via Hub.")
	assert.Contains(t, stderr, "Phase: running")
}

func TestStartAgentViaHub_AsyncFailure(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse("broker is near capacity"),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "error", Template: "claude",
				Launch: &hubclient.AgentLaunch{ID: "l1", State: "ended", Kind: "create", Error: "unschedulable"}},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	var failed *launchFailedError
	require.ErrorAs(t, err, &failed)
	assert.Contains(t, stderr, "Warning: broker is near capacity", "warnings are printed when the wait fails")
	assert.Contains(t, err.Error(), "create did not complete (unschedulable)")
	assert.Contains(t, err.Error(), "scion delete a1")
}

func TestStartAgentViaHub_InFlight200WithWarningsWaits(t *testing.T) {
	// Re-running start on an agent that is already launching: the Hub
	// answers 200 with the current agent and a warning; the CLI resumes
	// waiting.
	hub := &launchMockHub{t: t, createStatus: http.StatusOK,
		createBody: asyncCreateResponse("agent is already launching; request inputs were not applied"),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running"},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "new task", false, nil) })
	})
	require.NoError(t, err)
	assert.Equal(t, 1, hub.getsAfterCR)
	assert.Contains(t, stderr, "Warning: agent is already launching; request inputs were not applied")
	assert.Contains(t, stderr, "Agent 'a1' started via Hub.")
}

func TestStartAgentViaHub_NoWaitReturnsAfterAdmission(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse()}
	hubCtx := setupLaunchStartTest(t, hub)
	startNoWait = true
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	assert.Equal(t, 0, hub.getsAfterCR)
	assert.Contains(t, stderr, "Agent 'a1' accepted by Hub and launching.")
	assert.Contains(t, stderr, "Phase: provisioning")
}

func TestStartAgentViaHub_WaitTimeout(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse(),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning", Launch: activeLaunch("scheduling", 2, nil)},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	startWaitTimeout = 50 * time.Millisecond
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	var timeout *launchWaitTimeoutError
	require.ErrorAs(t, err, &timeout)
	assert.Contains(t, err.Error(), "re-run scion start a1 to keep waiting")
}

func TestStartAgentViaHub_CreateIncomplete409(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusConflict, createBody: map[string]interface{}{
		"error": map[string]interface{}{
			"code":    "agent_create_incomplete",
			"message": p1b3IncompleteCreateMessage,
			"details": map[string]interface{}{"template": "claude", "task": "fix the tests"},
		},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	var failed *launchFailedError
	require.ErrorAs(t, err, &failed)
	assert.Contains(t, err.Error(), "create did not complete (image_pull_failed)")
	assert.Contains(t, err.Error(), "Template: claude")
	assert.Contains(t, err.Error(), "Task: fix the tests")
	assert.NotContains(t, err.Error(), "scion hub disable")
	assert.Equal(t, 1, strings.Count(err.Error(), "create it again"), err.Error())
	assert.NotContains(t, err.Error(), "Delete the agent and create it again")
	assert.Equal(t, 0, hub.getsAfterCR)
}

func TestStartAgentViaHub_JSONOutputStaysClean(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse(),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)},
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running"},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	outputFormat = "json"
	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	var result ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), "stdout must be a single JSON document: %q", stdout)
	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "running", result.Details["phase"])
	assert.NotContains(t, stderr, "pod_create", "no progress output under --format json")
}

func TestAttachViaHub_LaunchingAgentHint(t *testing.T) {
	const projectID, agentName = "proj-attach-launch", "a1"
	agentPath := "/api/v1/projects/" + projectID + "/agents/" + agentName
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == agentPath:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID: "id-1", Name: agentName, Phase: "provisioning", RuntimeBrokerID: mockAttachBrokerID,
				Launch: activeLaunch("image_pull", 2, nil),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/"+mockAttachBrokerID:
			_ = json.NewEncoder(w).Encode(mockAttachBroker(""))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent 'a1' is still launching (phase: provisioning, step: image_pull)")
	assert.Contains(t, err.Error(), "scion start a1 --attach")
}

func TestWaitForAgentLaunch_BudgetFromFirstFetch(t *testing.T) {
	// With no explicit timeout and no create answer to seed the budget, the
	// first fetched agent's remainingSeconds sets it: 0s + slack (20ms).
	shortenLaunchWaitTimings(t)
	get := func(context.Context) (*hubclient.Agent, error) {
		return &hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("scheduling", 0, nil)}, nil
	}
	start := time.Now()
	_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{AgentName: "a1", Get: get})
	var timeout *launchWaitTimeoutError
	require.ErrorAs(t, err, &timeout)
	assert.Less(t, time.Since(start), 2*time.Second, "budget must come from the fetched agent, not the fallback")
}

func TestWaitForAgentLaunch_SIGTERMIsNotAnInterrupt(t *testing.T) {
	shortenLaunchWaitTimings(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	get := func(context.Context) (*hubclient.Agent, error) {
		cancel(waitSignalCause{sig: syscall.SIGTERM})
		return &hubclient.Agent{Phase: "provisioning", Launch: activeLaunch("pod_create", 2, nil)}, nil
	}
	_, err := waitForAgentLaunch(ctx, launchWaitOptions{AgentName: "a1", Get: get, PollInterval: time.Millisecond, Timeout: 3 * time.Second})
	var interrupted *launchWaitInterruptedError
	require.ErrorAs(t, err, &interrupted)
	assert.True(t, strings.HasPrefix(err.Error(), "terminated while waiting; agent 'a1' is still launching"), err.Error())
	assert.Equal(t, 143, exitCodeFor(err))

	// SIGINT recorded as the cause is a plain interrupt.
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	cancel2(waitSignalCause{sig: os.Interrupt})
	_, err = waitForAgentLaunch(ctx2, launchWaitOptions{AgentName: "a1", Get: get, PollInterval: time.Millisecond, Timeout: 3 * time.Second})
	assert.True(t, strings.HasPrefix(err.Error(), "stopped waiting;"), err.Error())
	assert.Equal(t, 130, exitCodeFor(err))
}

func TestExitCodeFor(t *testing.T) {
	assert.Equal(t, 1, exitCodeFor(errors.New("boom")))
	assert.Equal(t, 130, exitCodeFor(fmt.Errorf("start: %w", &launchWaitInterruptedError{Agent: "a1"})))
	assert.Equal(t, 143, exitCodeFor(fmt.Errorf("start: %w", &launchWaitInterruptedError{Agent: "a1", Signal: syscall.SIGTERM})))
	assert.Equal(t, 1, exitCodeFor(&launchWaitTimeoutError{Agent: "a1"}))
	assert.Equal(t, 1, exitCodeFor(&launchFailedError{Agent: "a1"}))
}

func TestValidateLaunchWaitFlags(t *testing.T) {
	orig := startWaitTimeout
	t.Cleanup(func() { startWaitTimeout = orig })
	for _, d := range []time.Duration{0, time.Second, 10 * time.Minute} {
		startWaitTimeout = d
		assert.NoError(t, validateLaunchWaitFlags())
	}
	startWaitTimeout = -time.Second
	err := validateLaunchWaitFlags()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--wait-timeout must not be negative")
}

// finalizeCreateResponse is a create answer for a workspace upload: the Hub
// dispatches the start only on finalize, so the answer predates it.
func finalizeCreateResponse(launch *hubclient.AgentLaunch) *hubclient.CreateAgentResponse {
	return &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Name: "a1", Phase: "created", Launch: launch},
	}
}

func TestFinishHubStart_FinalizeWaitsOnOldHub(t *testing.T) {
	hub := &launchMockHub{t: t, created: true, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning"},
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running"},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			err = finishHubStart(hubCtx, launchTestProjectID, "a1", false, finalizeCreateResponse(nil), nil, true)
		})
	})
	require.NoError(t, err)
	assert.Equal(t, 2, hub.getsAfterCR, "a finalize dispatches the start, so the CLI waits")
	assert.Contains(t, stderr, "Agent 'a1' started via Hub.")
	assert.Contains(t, stderr, "Phase: running")
}

func TestFinishHubStart_FinalizeBudgetFromFirstGet(t *testing.T) {
	// The create answer advertises 3s, but it predates the finalize; the
	// budget comes from the first GET (0s + 20ms slack).
	hub := &launchMockHub{t: t, created: true, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning", Launch: activeLaunch("scheduling", 0, nil)},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	startWaitTimeout = 0
	var err error
	start := time.Now()
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			err = finishHubStart(hubCtx, launchTestProjectID, "a1", false, finalizeCreateResponse(activeLaunch("", 3, nil)), nil, true)
		})
	})
	var timeout *launchWaitTimeoutError
	require.ErrorAs(t, err, &timeout)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestFinishHubStart_FinalizeNoWaitReportsCurrentPhase(t *testing.T) {
	hub := &launchMockHub{t: t, created: true, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning"},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	startNoWait = true
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			err = finishHubStart(hubCtx, launchTestProjectID, "a1", false, finalizeCreateResponse(nil), nil, true)
		})
	})
	require.NoError(t, err)
	assert.Equal(t, 1, hub.getsAfterCR)
	assert.Contains(t, stderr, "Agent 'a1' accepted by Hub and launching.")
	assert.Contains(t, stderr, "Phase: provisioning")
	assert.NotContains(t, stderr, "Phase: created")
	assert.NotContains(t, stderr, "started via Hub")
}

func TestFinishHubStart_FinalizeNoWaitFetchFailsOmitsPhase(t *testing.T) {
	hub := &launchMockHub{t: t, created: true} // GETs answer 500
	hubCtx := setupLaunchStartTest(t, hub)
	startNoWait = true
	outputFormat = "json"
	var err error
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			err = finishHubStart(hubCtx, launchTestProjectID, "a1", false, finalizeCreateResponse(nil), nil, true)
		})
	})
	require.NoError(t, err)
	var result ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	assert.Equal(t, "Agent 'a1' accepted by Hub and launching.", result.Message)
	_, hasPhase := result.Details["phase"]
	assert.False(t, hasPhase, "a stale phase is not reported")
}

func TestStartAgentViaHub_AttachOverridesNoWait(t *testing.T) {
	clearAppTokenSources(t)
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	t.Cleanup(func() { resolveAttachTransportFn = orig })

	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse(),
		afterCreate: []interface{}{
			hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running", RuntimeBrokerID: mockAttachBrokerID},
		}}
	hubCtx := setupLaunchStartTest(t, hub)
	attach, startNoWait = true, true
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	// The attach step itself stops at the token gate in this test.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no access token found for Hub")
	assert.Equal(t, 1, hub.getsAfterCR, "--attach waits for running even with --no-wait")
}

func TestStartAgentViaHub_AttachWithJSONDoesNotWait(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "provisioning"},
	}, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running"},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	attach, outputFormat = true, "json"
	var err error
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	assert.Equal(t, 0, hub.getsAfterCR, "JSON output returns after the JSON document, without waiting")
	var result ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	assert.Equal(t, "provisioning", result.Details["phase"])
}

func TestStartAgentViaHub_NoWaitJSONIncludesLaunch(t *testing.T) {
	deadline := time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)
	resp := asyncCreateResponse()
	resp.Agent.Launch = activeLaunch("", 2, &deadline)
	hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: resp}
	hubCtx := setupLaunchStartTest(t, hub)
	startNoWait, outputFormat = true, "json"
	var err error
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.NoError(t, err)
	var result ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), stdout)
	assert.Equal(t, "Agent 'a1' accepted by Hub and launching.", result.Message)
	assert.Equal(t, "launch-1", result.Details["launchId"])
	assert.Equal(t, "2026-10-03T04:00:00Z", result.Details["launchDeadline"])
	assert.Equal(t, "provisioning", result.Details["phase"])
}

// recordSignalRegistration replaces signalNotify/signalStop with recorders.
func recordSignalRegistration(t *testing.T) (notified, stopped *[]chan<- os.Signal) {
	t.Helper()
	origNotify, origStop := signalNotify, signalStop
	t.Cleanup(func() { signalNotify, signalStop = origNotify, origStop })
	var n, s []chan<- os.Signal
	var mu sync.Mutex
	signalNotify = func(c chan<- os.Signal, sig ...os.Signal) {
		mu.Lock()
		defer mu.Unlock()
		n = append(n, c)
	}
	signalStop = func(c chan<- os.Signal) {
		mu.Lock()
		defer mu.Unlock()
		s = append(s, c)
	}
	return &n, &s
}

func TestStartAgentViaHub_WaitReleasesSignalHandler(t *testing.T) {
	cases := []struct {
		name    string
		after   hubclient.Agent
		wantErr bool
	}{
		{"running", hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running"}, false},
		{"failed", hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "error"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notified, stopped := recordSignalRegistration(t)
			hub := &launchMockHub{t: t, createStatus: http.StatusCreated, createBody: asyncCreateResponse(),
				afterCreate: []interface{}{tc.after}}
			hubCtx := setupLaunchStartTest(t, hub)
			var err error
			_ = captureStderr(t, func() {
				_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
			})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, *notified, 1, "the wait registers one signal handler")
			require.Len(t, *stopped, 1, "the signal handler is released when the wait ends")
			assert.Equal(t, (*notified)[0], (*stopped)[0])
		})
	}
}

func TestFinishHubStart_JSONAttachAfterFinalizeAttaches(t *testing.T) {
	clearAppTokenSources(t)
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	t.Cleanup(func() { resolveAttachTransportFn = orig })

	hub := &launchMockHub{t: t, created: true, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running", RuntimeBrokerID: mockAttachBrokerID},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	attach, outputFormat = true, "json"
	var err error
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			err = finishHubStart(hubCtx, launchTestProjectID, "a1", false, finalizeCreateResponse(nil), nil, true)
		})
	})
	// The attach step stops at the token gate in this test.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no access token found for Hub")
	assert.Equal(t, 1, hub.getsAfterCR)
	assert.Empty(t, stdout, "no JSON document before the attach")
}

func TestWaitForAgentLaunch_ClientErrorStopsAtOnce(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		agentID  string
		wantHint bool
	}{
		{"401 user", http.StatusUnauthorized, "", true},
		{"403 user", http.StatusForbidden, "", true},
		{"401 hub-managed agent", http.StatusUnauthorized, "agent-123", false},
		{"400", http.StatusBadRequest, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortenLaunchWaitTimings(t)
			t.Setenv("SCION_AGENT_ID", tc.agentID)
			apiErr := &apiclient.APIError{StatusCode: tc.status, Code: "rejected", Message: "request rejected"}
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				errResult(apiErr),
				agentResult(&hubclient.Agent{Phase: "running"}),
			}}
			_, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
			})
			require.Error(t, err)
			assert.Equal(t, 1, seq.calls, "a client error is not retried")
			assert.ErrorIs(t, err, apiErr, "the Hub's error is kept")
			assert.Contains(t, err.Error(), "stopped waiting for agent 'a1': the Hub refused the status request")
			assert.Contains(t, err.Error(), "the launch continues on the Hub")
			if tc.wantHint {
				assert.Contains(t, err.Error(), "scion hub auth login")
			} else {
				assert.NotContains(t, err.Error(), "scion hub auth login")
			}
			assert.Equal(t, 1, exitCodeFor(err))
		})
	}
}

func TestWaitForAgentLaunch_RetryableErrorsKeepPolling(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusRequestTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			shortenLaunchWaitTimings(t)
			seq := &agentSequence{results: []func() (*hubclient.Agent, error){
				errResult(&apiclient.APIError{StatusCode: status, Code: "retry", Message: "try again"}),
				agentResult(&hubclient.Agent{Phase: "running"}),
			}}
			a, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
				AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second,
			})
			require.NoError(t, err)
			require.NotNil(t, a)
			assert.Equal(t, "running", a.Phase)
			assert.Equal(t, 2, seq.calls)
		})
	}
}
