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

// This file covers the HTTP lifecycle-handler level: a start leg
// dispatched against an agent whose phase is "running" must not revoke by
// agent on failure, because the container that phase describes may
// genuinely still be up. Two handler paths reach DispatchAgentStart with
// agent.Phase == "running": the user-facing Restart action (stop-then-start,
// where the stop leg may report no running instance), and the Start action
// dispatched again at an already-running agent. See httpdispatcher_credential_revoke_test.go
// for the dispatcher-level unit tests covering the same guard directly.
package hub

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentLifecycle_Restart_StopFailureLeavesCredentialActive covers a
// restart whose stop leg fails: the container may still be running, so the
// restart aborts before the start leg (ptone/scion#2710), mints nothing, and
// must not revoke the running agent's credential.
func TestAgentLifecycle_Restart_StopFailureLeavesCredentialActive(t *testing.T) {
	srv, s := testServer(t)

	agent := setupBrokerAgentInPhase(t, s, "restart-both-fail", state.PhaseRunning)

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker unreachable")}
	dispatcher := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	// Stands in for the credential the still-running container is actively
	// using.
	insertTestAgentCredential(t, s, agent.ID, agent.ProjectID, "restart-both-fail-preexisting-jti")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	preexisting := getTestAgentCredential(t, s, "restart-both-fail-preexisting-jti")
	assert.Nil(t, preexisting.RevokedAt, "a restart whose stop leg fails must not revoke a running agent's credential")
	assert.Empty(t, gen.jtis, "the start leg is never dispatched, so nothing is minted")
}

// TestAgentLifecycle_Start_RunningPhaseFailureLeavesCredentialActive covers
// the lifecycle Start action dispatched against an agent
// that is already running (per the handler's own comment allowing "start
// called again on an already-running agent"); a failure there must not
// revoke the running agent's credential.
func TestAgentLifecycle_Start_RunningPhaseFailureLeavesCredentialActive(t *testing.T) {
	srv, s := testServer(t)

	agent := setupBrokerAgentInPhase(t, s, "start-running-fail", state.PhaseRunning)

	mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker unreachable")}
	dispatcher := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	insertTestAgentCredential(t, s, agent.ID, agent.ProjectID, "start-running-fail-preexisting-jti")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	preexisting := getTestAgentCredential(t, s, "start-running-fail-preexisting-jti")
	assert.Nil(t, preexisting.RevokedAt, "a failed start dispatched against an already-running agent must not revoke its credential")

	require.Len(t, gen.jtis, 1, "the start dispatch still mints its own credential before failing")
	minted := getTestAgentCredential(t, s, gen.lastJTI())
	assert.Nil(t, minted.RevokedAt, "the credential minted for the failed start must also stay active")
}

// TestAgentLifecycle_Start_TransitionalPhaseFailureLeavesCredentialActive
// covers the lifecycle Start action, which has no phase guard, dispatched
// against an agent in one of the two transitional phases a live container
// can still be up under: "starting" (wake_dm.go leaves a resumed agent here
// after a readiness-wait timeout) and "stopping" (reincarnate_worker.go
// writes this before its own stop dispatch has confirmed the prior
// container actually exited). A failure there must not revoke the agent's
// credential.
func TestAgentLifecycle_Start_TransitionalPhaseFailureLeavesCredentialActive(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseStarting, state.PhaseStopping} {
		t.Run(string(phase), func(t *testing.T) {
			srv, s := testServer(t)

			agent := setupBrokerAgentInPhase(t, s, "start-transitional-"+string(phase), phase)

			mockClient := &mockRuntimeBrokerClient{returnErr: errors.New("broker unreachable")}
			dispatcher := NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: s}
			dispatcher.SetTokenGenerator(gen)
			srv.SetDispatcher(dispatcher)

			insertTestAgentCredential(t, s, agent.ID, agent.ProjectID, "start-transitional-"+string(phase)+"-preexisting-jti")

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

			preexisting := getTestAgentCredential(t, s, "start-transitional-"+string(phase)+"-preexisting-jti")
			assert.Nil(t, preexisting.RevokedAt, "a failed start dispatched against a transitional-phase agent must not revoke its credential")

			require.Len(t, gen.jtis, 1, "the start dispatch still mints its own credential before failing")
			minted := getTestAgentCredential(t, s, gen.lastJTI())
			assert.Nil(t, minted.RevokedAt, "the credential minted for the failed start must also stay active")
		})
	}
}
