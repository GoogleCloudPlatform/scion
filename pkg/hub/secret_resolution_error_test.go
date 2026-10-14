// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeSecretResolutionErr is the dispatch error buildStartEnv returns on a
// secret resolution error, wrapping a backend error that names the fake
// secret.
func fakeSecretResolutionErr(verb string) error {
	return &secretResolutionError{Verb: verb, Err: errors.New(fakeResolveBackendError + ": " + fakeFileSecret.Name)}
}

// lifecycleErrorBody returns the error code and message of a lifecycle
// response body, and the body re-encoded for detail checks.
func lifecycleErrorBody(t *testing.T, body map[string]interface{}) (code, message, raw string) {
	t.Helper()
	e, _ := body["error"].(map[string]interface{})
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	data, err := json.Marshal(body)
	require.NoError(t, err)
	return code, message, string(data)
}

func TestSecretResolutionError_FixedMessageAndUnwrap(t *testing.T) {
	backendErr := errors.New(fakeResolveBackendError)
	err := fmt.Errorf("dispatch start: %w", &secretResolutionError{Verb: "started", Err: backendErr})
	assert.ErrorIs(t, err, backendErr, "the backend error stays reachable for errors.Is")
	assert.True(t, isSecretResolutionError(err))
	assert.Equal(t, "dispatch start: agent secrets could not be resolved; the agent was not started", err.Error())
	assertNoSecretResolutionDetail(t, err.Error(), "the error text")
	assert.True(t, startDidNotHappen(err))
	assert.Equal(t, startReleased, startOutcomeOf(err))
	assert.False(t, isSecretResolutionError(errors.New("other")))
}

// A start of a stopped agent that fails on a secret resolution error is a
// 503 unavailable with the fixed message; the claim is released and the
// agent keeps its phase.
func TestSecretResolutionError_StartIs503(t *testing.T) {
	f, d, a := newClaimFixture(t)
	d.start = func(context.Context, *store.Agent) error { return fakeSecretResolutionErr("started") }

	status, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", body)
	code, message, raw := lifecycleErrorBody(t, body)
	assert.Equal(t, ErrCodeUnavailable, code)
	assert.Equal(t, "agent secrets could not be resolved; the agent was not started", message)
	assertNoSecretResolutionDetail(t, raw, "the API body")

	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "stopped", got.Phase)
	assert.Empty(t, got.StartClaimID, "the start did not happen: the claim is released")
	assertNoSecretResolutionDetail(t, got.Message, "the agent message")
}

// A provisioned agent whose start fails on a secret resolution error goes
// back to rest with the fixed message, like any start that did not happen.
func TestSecretResolutionError_ProvisionedStartReturnsToRest(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	a := f.addAgent("provisioned", "created", "")
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	d.start = func(context.Context, *store.Agent) error { return fakeSecretResolutionErr("started") }

	status, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", body)
	_, _, raw := lifecycleErrorBody(t, body)
	assertNoSecretResolutionDetail(t, raw, "the API body")

	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "created", got.Phase)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "back at rest")
	assert.Equal(t, "Start failed: agent secrets could not be resolved; the agent was not started. "+provisionedRestingNote, got.Message)
	assertNoSecretResolutionDetail(t, got.Message, "the agent message")
	assert.Empty(t, got.StartClaimID)
}

// A restart whose start leg fails on a secret resolution error is a 503
// with the fixed message, after the stop leg ran; the claim is released and
// the agent ends stopped. The error comes either straight from a local
// dispatch or rebuilt from a failed cross-node row (wrapped, with a nil Err).
func TestSecretResolutionError_RestartIs503(t *testing.T) {
	cases := []struct {
		name string
		err  func() error
	}{
		{"local", func() error { return fakeSecretResolutionErr("started") }},
		{"rebuilt from a cross-node row", func() error {
			return dispatchFailureError(&store.BrokerDispatch{
				Op:     "start",
				Error:  "dispatch start: agent secrets could not be resolved; the agent was not started",
				Result: `{"secretResolution":true}`,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, _ := newClaimFixture(t)
			a := f.addAgent("restarting", "running", "working")
			// The agent has a run, as every dispatched agent does: a failed
			// restart records the stopped state only against the current run.
			_, err := f.s.SetAgentRunID(context.Background(), a.ID, "run-before-restart", nil)
			require.NoError(t, err)
			d.start = func(context.Context, *store.Agent) error { return tc.err() }

			status, body := lifecycle(t, f, a.ID, "restart")
			require.Equal(t, http.StatusServiceUnavailable, status, "%v", body)
			code, message, raw := lifecycleErrorBody(t, body)
			assert.Equal(t, ErrCodeUnavailable, code)
			assert.Equal(t, "agent secrets could not be resolved; the agent was not started", message)
			assertNoSecretResolutionDetail(t, raw, "the API body")
			assert.Equal(t, int32(1), d.stops.Load(), "the stop leg ran")

			got := getAgent(t, f.s, a.ID)
			assert.Equal(t, "stopped", got.Phase, "the stop leg ran and the start leg did not: the agent ends stopped")
			assert.Empty(t, got.StartClaimID)
			assertNoSecretResolutionDetail(t, got.Message, "the agent message")
		})
	}
}

// Waking a suspended agent for a DM through the real dispatcher, whose
// secret backend fails to resolve: 503 unavailable with the fixed message,
// no broker call, and the agent back at rest with its claim released.
func TestWakeAgentForDM_SecretResolutionError_503(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "wake-secrets")
	agent := newQuotaTestAgent(t, s, broker, project, "wake-secrets", state.PhaseSuspended)

	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetSecretBackend(&failingResolveBackend{})
	srv.SetDispatcher(d)

	_, dmErr := srv.wakeAgentForDM(context.Background(), agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus, dmErr.Message)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, "agent secrets could not be resolved; the agent was not started", dmErr.Message)
	assertNoSecretResolutionDetail(t, dmErr.Message, "the DM error")
	assert.False(t, client.startCalled || client.createCalled, "broker must not be called")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the agent is back at rest")
	assert.Empty(t, got.StartClaimID)
	assertNoSecretResolutionDetail(t, got.Message, "the agent message")
}

// The secret-resolution marker round-trips on a failed dispatch row: the
// requesting node rebuilds the typed error with the verb of the row's op,
// and neither the row nor the rebuilt error carries the backend error or
// the secret name. A row without the marker rebuilds no such error.
func TestDispatchFailureResult_SecretResolution(t *testing.T) {
	for _, tc := range []struct{ op, verb string }{{"start", "started"}, {"restart", "restarted"}} {
		t.Run(tc.op, func(t *testing.T) {
			execErr := fmt.Errorf("dispatch %s: %w", tc.op, fakeSecretResolutionErr(tc.verb))
			result := dispatchFailureResult(execErr)
			env := decodeDispatchFailure(result)
			require.NotNil(t, env)
			assert.True(t, env.SecretResolution)
			assert.Nil(t, env.BrokerError)
			assertNoSecretResolutionDetail(t, result, "the row result")
			assertNoSecretResolutionDetail(t, execErr.Error(), "the row error text")

			rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: tc.op, Error: execErr.Error(), Result: result})
			var got *secretResolutionError
			require.ErrorAs(t, rebuilt, &got)
			assert.Equal(t, tc.verb, got.Verb)
			assert.Equal(t, "dispatch "+tc.op+" failed: agent secrets could not be resolved; the agent was not "+tc.verb, rebuilt.Error())
			assertNoSecretResolutionDetail(t, rebuilt.Error(), "the rebuilt error")
			assert.True(t, isConfirmedStartNotActedOnError(rebuilt))
			assert.Equal(t, startReleased, startOutcomeOf(rebuilt))
			assert.True(t, reincarnationStartLeftNoContainer(rebuilt))
		})
	}

	other := dispatchFailureResult(fmt.Errorf("x: %w", store.ErrDeleteInProgress))
	assert.NotContains(t, other, "secretResolution")
	assert.False(t, isSecretResolutionError(dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: other})))
	assert.Empty(t, dispatchFailureResult(errors.New("plain")), "an unclassified error still records no envelope")
}

// A queued start and a queued restart whose owning node fails secret
// resolution reach the requesting node as the typed error, ending its wait
// on the row's failure.
func TestCrossNodeLifecycle_SecretResolutionError(t *testing.T) {
	cases := []struct {
		name, verb string
		call       func(ctx context.Context, f *crossNodeFixture) error
	}{
		{"start", "started", func(ctx context.Context, f *crossNodeFixture) error {
			return f.requester.DispatchAgentStart(ctx, f.agent, "", false)
		}},
		{"restart", "restarted", func(ctx context.Context, f *crossNodeFixture) error {
			return f.requester.DispatchAgentRestart(ctx, f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossNodeFixture(t, fakeSecretResolutionErr(tc.verb), true)
			err := tc.call(crossNodeCtx(t), f)
			var got *secretResolutionError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, tc.verb, got.Verb)
			assertNoSecretResolutionDetail(t, err.Error(), "the requester's error")
			assert.Equal(t, startReleased, startOutcomeOf(err))
		})
	}
}

// Through the lifecycle API: a start whose owning node fails secret
// resolution answers 503 unavailable with the fixed message and releases
// the claim. A user restart's start leg is this same queued start; the
// restart's stopped state is covered by TestSecretResolutionError_RestartIs503
// (this fixture's owner publishes no stop status, so a cross-node stop leg
// would only wait out its timeout), and the queued restart op by
// TestCrossNodeLifecycle_SecretResolutionError.
func TestCrossNodeHandler_SecretResolutionErrorIs503(t *testing.T) {
	srv, agent := crossNodeHandlerServer(t, fakeSecretResolutionErr("started"), state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+string(api.AgentActionStart), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeUnavailable, code)
	assert.Contains(t, rec.Body.String(), "agent secrets could not be resolved; the agent was not started")
	assertNoSecretResolutionDetail(t, rec.Body.String(), "the API body")
	got := getAgent(t, srv.store, agent.ID)
	assert.Empty(t, got.StartClaimID, "the start did not happen: the claim is released")
	assertNoSecretResolutionDetail(t, got.Message, "the agent message")
}
