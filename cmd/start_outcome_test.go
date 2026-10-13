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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// The interrupt path deletes only before the env is submitted, and the
// interrupt and error paths together delete at most once.
func TestEnvGatherCleanupGuard(t *testing.T) {
	var deletes int
	c := &envGatherCleanup{del: func() { deletes++ }}
	c.markSubmitted()
	assert.False(t, c.cleanupIfUnsubmitted(), "no delete after the env was submitted")
	assert.Equal(t, 0, deletes)

	c2 := &envGatherCleanup{del: func() { deletes++ }}
	assert.True(t, c2.cleanupIfUnsubmitted())
	assert.False(t, c2.cleanupAfterFailure(errors.New("local failure")), "a second delete must not run")
	assert.Equal(t, 1, deletes)

	c3 := &envGatherCleanup{del: func() { deletes++ }}
	c3.markSubmitted()
	assert.False(t, c3.cleanupAfterFailure(&envSubmitError{err: errors.New("EOF")}), "no delete on an unknown submit outcome")
	assert.True(t, c3.cleanupAfterFailure(&envSubmitError{err: &apiclient.APIError{StatusCode: http.StatusBadRequest}}), "a refused submit deletes")
	assert.Equal(t, 2, deletes)
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
