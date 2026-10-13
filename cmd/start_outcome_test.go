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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startOutcomeHub is a fake Hub for the start-outcome tests. The create
// answers 202 with an env-gather need for START_OUTCOME_KEY (set locally),
// unless createHandler replaces it. submitHandler answers the env submit.
// agentGets counts GETs of the agent; the first answers 404 (the
// pre-create existing-agent check) and later ones answer agentAfter, or
// 404 when it is nil.
type startOutcomeHub struct {
	projectID     string
	agentID       string
	createHandler http.HandlerFunc
	submitHandler http.HandlerFunc
	agentAfter    map[string]interface{}
	deletes       atomic.Int32
	agentGets     atomic.Int32
}

func (h *startOutcomeHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	agentsPath := "/api/v1/projects/" + h.projectID + "/agents"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == agentsPath:
			if h.createHandler != nil {
				h.createHandler(w, r)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{"id": h.agentID, "name": "outcome-agent", "phase": "provisioning"},
				"envGather": map[string]interface{}{
					"agentId":  h.agentID,
					"required": []string{"START_OUTCOME_KEY"},
					"needs":    []string{"START_OUTCOME_KEY"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == agentsPath+"/"+h.agentID+"/env":
			h.submitHandler(w, r)
		case r.Method == http.MethodDelete:
			h.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == agentsPath+"/outcome-agent":
			n := h.agentGets.Add(1)
			if n == 1 || h.agentAfter == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "not_found", "message": "Agent not found"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(h.agentAfter)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// setupStartOutcomeTest sets the package state the hub start path reads
// and returns a HubContext for server with a short client timeout.
func setupStartOutcomeTest(t *testing.T, serverURL, projectID string) *HubContext {
	t.Helper()
	origNonInteractive, origAutoConfirm, origOutputFormat := nonInteractive, autoConfirm, outputFormat
	origLookup := startOutcomeLookupTimeout
	t.Cleanup(func() {
		nonInteractive, autoConfirm, outputFormat = origNonInteractive, origAutoConfirm, origOutputFormat
		startOutcomeLookupTimeout = origLookup
	})
	nonInteractive = true
	autoConfirm = true
	outputFormat = "json"
	startOutcomeLookupTimeout = 2 * time.Second
	// A user caller: a 404 is a definite not-found.
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv("START_OUTCOME_KEY", "value")

	client, err := hubclient.New(serverURL, hubclient.WithTimeout(300*time.Millisecond))
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: serverURL, ProjectID: projectID}
}

func writeAPIError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": code},
	})
}

var launchingAgent = map[string]interface{}{
	"id": "agent-outcome", "name": "outcome-agent", "phase": "provisioning",
	"launch": map[string]interface{}{"id": "l1", "state": "active", "active": true},
}

// An env submit that times out on the client may have started the agent on
// the Hub, which runs it detached: the CLI must not delete it, and reports
// that it may have started (ptone/scion#4007).
func TestStartAgentViaHub_EnvSubmitTimeoutDoesNotDelete(t *testing.T) {
	release := make(chan struct{})
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome", agentAfter: launchingAgent}
	h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		writeAPIError(w, http.StatusOK, "late")
	}
	server := h.serve(t)
	defer server.Close()
	defer close(release)
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), h.deletes.Load(), "an env submit with an unknown outcome must not delete the agent")
	assert.Contains(t, err.Error(), "may have started")
	assert.Contains(t, err.Error(), "phase provisioning, launch active")
}

// A 5xx answer to the env submit is not a definite refusal: the Hub may
// have launched the agent before failing (for example its own dispatch
// deadline).
func TestStartAgentViaHub_EnvSubmitServerErrorDoesNotDelete(t *testing.T) {
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome", agentAfter: launchingAgent}
	h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusBadGateway, "runtime_error")
	}
	server := h.serve(t)
	defer server.Close()
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), h.deletes.Load(), "a 5xx env submit must not delete the agent")
	assert.Contains(t, err.Error(), "may have started")
}

// A 409 to the env submit means another operation owns the agent (it left
// provisioning, a start or launch holds it, or a delete is in progress).
func TestStartAgentViaHub_EnvSubmitConflictDoesNotDelete(t *testing.T) {
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome", agentAfter: map[string]interface{}{
		"id": "agent-outcome", "name": "outcome-agent", "phase": "running",
	}}
	h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusConflict, "invalid_state")
	}
	server := h.serve(t)
	defer server.Close()
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), h.deletes.Load(), "a 409 env submit must not delete the agent")
	assert.Contains(t, err.Error(), "phase running")
}

// A definite refusal of the env submit (a 4xx other than 408, 409 and 429)
// means nothing was launched: the provisioning agent is cleaned up as before.
func TestStartAgentViaHub_EnvSubmitRefusalCleansUp(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome", agentAfter: launchingAgent}
			h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
				writeAPIError(w, status, "refused")
			}
			server := h.serve(t)
			defer server.Close()
			hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

			err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
			require.Error(t, err)
			assert.Equal(t, int32(1), h.deletes.Load(), "a refused env submit cleans up the agent")
			assert.Contains(t, err.Error(), "env-gather failed")
			assert.NotContains(t, err.Error(), "may have started")
		})
	}
}

func TestEnvGatherCleanupSafe(t *testing.T) {
	submit := func(status int) error {
		return &envSubmitError{err: &apiclient.APIError{StatusCode: status, Code: "x", Message: "x"}}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"before submit", errors.New("cannot satisfy required environment variables"), true},
		{"400", submit(http.StatusBadRequest), true},
		{"401", submit(http.StatusUnauthorized), true},
		{"403", submit(http.StatusForbidden), true},
		{"404", submit(http.StatusNotFound), true},
		{"412", submit(http.StatusPreconditionFailed), true},
		{"422", submit(http.StatusUnprocessableEntity), true},
		{"408", submit(http.StatusRequestTimeout), false},
		{"409", submit(http.StatusConflict), false},
		{"429", submit(http.StatusTooManyRequests), false},
		{"500", submit(http.StatusInternalServerError), false},
		{"502", submit(http.StatusBadGateway), false},
		{"503", submit(http.StatusServiceUnavailable), false},
		{"transport", &envSubmitError{err: errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")}, false},
		{"wrapped transport", fmt.Errorf("outer: %w", &envSubmitError{err: errors.New("EOF")}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, envGatherCleanupSafe(tc.err))
		})
	}
}

// The interrupt path deletes only before the env is submitted; the error
// path deletes before the submit or after a definite refusal; at most one
// delete runs.
func TestEnvGatherCleanupGuard(t *testing.T) {
	var deletes atomic.Int32
	del := func() { deletes.Add(1) }

	c := newEnvGatherCleanup(del)
	require.True(t, c.markSubmitted())
	assert.False(t, c.cleanupIfUnsubmitted(), "no delete after the env was submitted")
	assert.Equal(t, int32(0), deletes.Load())

	c2 := newEnvGatherCleanup(del)
	assert.True(t, c2.cleanupIfUnsubmitted())
	require.False(t, c2.markSubmitted(), "no submit once the delete began")
	assert.False(t, c2.cleanupAfterFailure(errors.New("local failure")), "a second delete must not run")
	assert.Equal(t, int32(1), deletes.Load())

	c3 := newEnvGatherCleanup(del)
	require.True(t, c3.markSubmitted())
	assert.False(t, c3.cleanupAfterFailure(&envSubmitError{err: errors.New("EOF")}), "no delete on an unknown submit outcome")
	assert.True(t, c3.cleanupAfterFailure(&envSubmitError{err: &apiclient.APIError{StatusCode: http.StatusBadRequest}}), "a refused submit deletes")
	assert.False(t, c3.cleanupIfUnsubmitted())
	assert.Equal(t, int32(2), deletes.Load())
}

// The interrupt's delete and the env submit are mutually exclusive: in no
// interleaving do both happen.
func TestEnvGatherCleanupGuard_InterruptRacesSubmit(t *testing.T) {
	for i := 0; i < 500; i++ {
		var deleted atomic.Bool
		c := newEnvGatherCleanup(func() { deleted.Store(true) })
		start := make(chan struct{})
		var submitted, interrupted bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; submitted = c.markSubmitted() }()
		go func() { defer wg.Done(); <-start; interrupted = c.cleanupIfUnsubmitted() }()
		close(start)
		wg.Wait()
		require.NotEqual(t, submitted, interrupted, "exactly one of submit and interrupt delete must win (iteration %d)", i)
		require.Equal(t, interrupted, deleted.Load())
	}
}

// The submit marks the cleanup submitted, so a later interrupt leaves the
// agent alone.
func TestGatherAndSubmitEnvGuarded_MarksSubmitted(t *testing.T) {
	origNonInteractive, origAutoConfirm := nonInteractive, autoConfirm
	t.Cleanup(func() { nonInteractive, autoConfirm = origNonInteractive, origAutoConfirm })
	nonInteractive, autoConfirm = true, true
	t.Setenv("START_OUTCOME_KEY", "value")

	projectID := tid("project-1")
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	var deletes atomic.Int32
	c := newEnvGatherCleanup(func() { deletes.Add(1) })
	_, err = gatherAndSubmitEnvGuarded(context.Background(), hubCtx, projectID, startOutcomeEnvGatherResp(), c)
	require.NoError(t, err)
	require.Equal(t, "value", (*captured)["START_OUTCOME_KEY"])
	assert.False(t, c.cleanupIfUnsubmitted(), "an interrupt after the submit must not delete")
	assert.Equal(t, int32(0), deletes.Load())
}

// An interrupt that began the delete first stops the submit; a context
// that already ended sends nothing. Both are failures before the submit,
// so the agent is cleaned up.
func TestGatherAndSubmitEnvGuarded_NothingSent(t *testing.T) {
	origNonInteractive, origAutoConfirm := nonInteractive, autoConfirm
	t.Cleanup(func() { nonInteractive, autoConfirm = origNonInteractive, origAutoConfirm })
	nonInteractive, autoConfirm = true, true
	t.Setenv("START_OUTCOME_KEY", "value")

	projectID := tid("project-1")
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	interrupted := newEnvGatherCleanup(nil)
	require.True(t, interrupted.cleanupIfUnsubmitted())
	_, err = gatherAndSubmitEnvGuarded(context.Background(), hubCtx, projectID, startOutcomeEnvGatherResp(), interrupted)
	require.ErrorIs(t, err, errEnvGatherInterrupted)
	assert.True(t, envGatherCleanupSafe(err))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = gatherAndSubmitEnvGuarded(ctx, hubCtx, projectID, startOutcomeEnvGatherResp(), newEnvGatherCleanup(nil))
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, envGatherCleanupSafe(err), "a submit that was never sent is a failure before the submit")
	assert.Empty(t, *captured, "nothing may be submitted")
}

func startOutcomeEnvGatherResp() *hubclient.CreateAgentResponse {
	return &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
			Required: []string{"START_OUTCOME_KEY"},
			Needs:    []string{"START_OUTCOME_KEY"},
		},
	}
}

// The lookup after an unknown submit outcome: an agent caller's 404 may be
// a read it is not allowed (ptone/scion#4568), so it is not reported as
// gone; a user's 404 is; other failures are reported as unreadable.
func TestEnvSubmitOutcomeUnknownError_Lookup(t *testing.T) {
	submitErr := &envSubmitError{err: errors.New("request failed: timeout")}
	notFound := &apiclient.APIError{StatusCode: http.StatusNotFound, Code: "not_found", Message: "Agent not found"}
	getErr := func(err error) func(context.Context) (*hubclient.Agent, error) {
		return func(context.Context) (*hubclient.Agent, error) { return nil, err }
	}
	cases := []struct {
		name     string
		agentID  string
		get      func(context.Context) (*hubclient.Agent, error)
		contains []string
		absent   []string
	}{
		{"agent caller 404", "agent-x", getErr(notFound),
			[]string{"may have started", "could not be read"}, []string{"no longer exists"}},
		{"user caller 404", "", getErr(notFound),
			[]string{"no longer exists"}, []string{"may have started"}},
		{"server error", "", getErr(&apiclient.APIError{StatusCode: http.StatusInternalServerError, Code: "internal_error", Message: "boom"}),
			[]string{"may have started", "could not be read (", "boom"}, []string{"no longer exists"}},
		{"transport error", "", getErr(errors.New("request failed: EOF")),
			[]string{"could not be read (request failed: EOF)"}, []string{"no longer exists"}},
		{"phase from status", "", func(context.Context) (*hubclient.Agent, error) {
			return &hubclient.Agent{Name: "a", Status: "running"}, nil
		}, []string{"it is in phase running"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_AGENT_ID", tc.agentID)
			msg := envSubmitOutcomeUnknownError("a", submitErr, tc.get).Error()
			for _, c := range tc.contains {
				assert.Contains(t, msg, c)
			}
			for _, a := range tc.absent {
				assert.NotContains(t, msg, a)
			}
		})
	}
}

// A submit refused because a delete is in progress does not suggest
// another delete.
func TestEnvSubmitOutcomeUnknownError_DeleteInProgress(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	err := &envSubmitError{err: &apiclient.APIError{StatusCode: http.StatusConflict, Code: apiclient.ErrCodeDeleteInProgress, Message: "deleting"}}
	msg := envSubmitOutcomeUnknownError("a", err, func(context.Context) (*hubclient.Agent, error) {
		return &hubclient.Agent{Name: "a", Phase: "stopping"}, nil
	}).Error()
	assert.Contains(t, msg, "a delete of the agent is in progress")
	assert.NotContains(t, msg, "scion delete")
}

// End to end as an agent caller: the submit times out and the lookup gets
// 404, which for an agent may be a read it is not allowed. The agent is not
// deleted and not reported as gone.
func TestStartAgentViaHub_EnvSubmitTimeoutAgentCallerNotFound(t *testing.T) {
	release := make(chan struct{})
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome"} // every GET answers 404
	h.submitHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		writeAPIError(w, http.StatusOK, "late")
	}
	server := h.serve(t)
	defer server.Close()
	defer close(release)
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)
	t.Setenv("SCION_AGENT_ID", "agent-launcher")

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Equal(t, int32(0), h.deletes.Load())
	assert.Contains(t, err.Error(), "may have started")
	assert.NotContains(t, err.Error(), "no longer exists")
}

// The create-path lookup runs only for a transport failure: a local
// failure sent nothing. An agent that existed before the start is "being
// started", not created.
func TestCreateOutcomeUnknownError(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	var gets int
	get := func(context.Context) (*hubclient.Agent, error) {
		gets++
		return &hubclient.Agent{Name: "a", Phase: "stopped"}, nil
	}
	assert.NoError(t, createOutcomeUnknownError("a", errors.New("operation cancelled"), true, get))
	assert.NoError(t, createOutcomeUnknownError("a", &apiclient.APIError{StatusCode: http.StatusBadGateway}, false, get))
	assert.Equal(t, 0, gets, "no lookup for a local failure or a Hub answer")

	timeout := fmt.Errorf("request failed: %w", &url.Error{Op: "Post", URL: "http://hub/agents", Err: context.DeadlineExceeded})
	err := createOutcomeUnknownError("a", timeout, true, get)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still be starting it: phase stopped")
	err = createOutcomeUnknownError("a", timeout, false, get)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still be creating it")
}

// A create that times out on the client may still be running on the Hub
// (ptone/scion#4007): when the agent now exists, the error says so.
func TestStartAgentViaHub_CreateTimeoutReportsExistingAgent(t *testing.T) {
	release := make(chan struct{})
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome", agentAfter: map[string]interface{}{
		"id": "agent-outcome", "name": "outcome-agent", "phase": "created",
	}}
	h.createHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusCreated)
	}
	server := h.serve(t)
	defer server.Close()
	defer close(release)
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still be creating it")
	assert.Contains(t, err.Error(), "phase created")
	assert.Equal(t, int32(0), h.deletes.Load())
}

// When the timed-out create left no agent, the original error is kept.
func TestStartAgentViaHub_CreateTimeoutAgentAbsentKeepsError(t *testing.T) {
	release := make(chan struct{})
	h := &startOutcomeHub{projectID: "p-outcome", agentID: "agent-outcome"}
	h.createHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusCreated)
	}
	server := h.serve(t)
	defer server.Close()
	defer close(release)
	hubCtx := setupStartOutcomeTest(t, server.URL, h.projectID)

	err := startAgentViaHub(nil, hubCtx, "outcome-agent", "", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start agent via Hub")
	assert.NotContains(t, err.Error(), "may still be creating it")
	assert.GreaterOrEqual(t, h.agentGets.Load(), int32(2), "the agent is looked up after the timeout")
}
