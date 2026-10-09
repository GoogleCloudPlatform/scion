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
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2710: a restart starts the agent only when its stop leg
// succeeded or the broker reported no running instance. Any other stop
// error aborts with a retryable 503 and never dispatches the start, since
// the old instance may still be running.
func TestAgentLifecycle_RestartStopErrorHandling(t *testing.T) {
	brokerErr := func(status int, code string) error {
		return &brokerStatusError{
			StatusCode: status,
			Body:       `{"error":{"code":"` + code + `","message":"m"}}`,
		}
	}
	tests := []struct {
		name       string
		stopErr    error
		wantStatus int
		wantCode   string
		wantStart  bool
		// wantBrokerCode is the expected details.brokerCode of the 503;
		// "" means no details.
		wantBrokerCode string
	}{
		{name: "clean stop", stopErr: nil, wantStatus: http.StatusOK, wantStart: true},
		{name: "agent not found", stopErr: brokerErr(http.StatusNotFound, ErrCodeAgentNotFound),
			wantStatus: http.StatusOK, wantStart: true},
		{name: "agent not running", stopErr: brokerErr(http.StatusConflict, ErrCodeAgentNotRunning),
			wantStatus: http.StatusOK, wantStart: true},
		{name: "generic error", stopErr: errors.New("broker unreachable"),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "broker 502", stopErr: brokerErr(http.StatusBadGateway, ErrCodeRuntimeError),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable,
			wantBrokerCode: ErrCodeRuntimeError},
		{name: "404 without agent_not_found", stopErr: brokerErr(http.StatusNotFound, ErrCodeNotFound),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable,
			wantBrokerCode: ErrCodeNotFound},
		// A tolerated code on the wrong status is not tolerated: the status
		// check is part of the rule.
		{name: "agent_not_found on 500", stopErr: brokerErr(http.StatusInternalServerError, ErrCodeAgentNotFound),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable,
			wantBrokerCode: ErrCodeAgentNotFound},
		{name: "agent_not_running on 404", stopErr: brokerErr(http.StatusNotFound, ErrCodeAgentNotRunning),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable,
			wantBrokerCode: ErrCodeAgentNotRunning},
		{name: "malformed broker code omitted", stopErr: brokerErr(http.StatusConflict, "Not A Code"),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "runtime unavailable", stopErr: brokerErr(http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable),
			wantStatus: http.StatusServiceUnavailable, wantCode: brokerCodeRuntimeUnavailable},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &runIntentDispatcher{stopErr: tc.stopErr}
			srv.SetDispatcher(disp)
			_, _, agent := setupOnlineBrokerAgent(t, s, fmt.Sprintf("restart-stop-%d", i))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.EqualValues(t, 1, disp.stops.Load(), "the stop leg is always dispatched")
			if tc.wantStart {
				assert.EqualValues(t, 1, disp.starts.Load(), "the start leg follows a stop that left no running instance")
				return
			}
			assert.EqualValues(t, 0, disp.starts.Load(), "a failed stop must not be followed by a start")
			assert.NotEmpty(t, rec.Header().Get("Retry-After"))
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.wantCode, body.Error.Code)
			if tc.wantCode == ErrCodeUnavailable {
				assert.Equal(t, "Restart not performed: the agent's current instance could not be stopped; retry later",
					body.Error.Message, "the message is fixed; the broker body is never relayed")
				if tc.wantBrokerCode == "" {
					assert.Empty(t, body.Error.Details)
				} else {
					assert.Equal(t, map[string]interface{}{"brokerCode": tc.wantBrokerCode}, body.Error.Details)
				}
			}

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase,
				"an aborted restart leaves the pre-restart phase: the instance may still be running")
		})
	}
}

// tolerableStopFailingStartDispatcher answers a restart's stop leg with a
// tolerated broker error (404 agent_not_found) and fails its start leg.
type tolerableStopFailingStartDispatcher struct {
	failingStartDispatcher
}

func (d *tolerableStopFailingStartDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stopCount.Add(1)
	return &brokerStatusError{
		StatusCode: http.StatusNotFound,
		Body:       `{"error":{"code":"` + ErrCodeAgentNotFound + `","message":"m"}}`,
	}
}

// A tolerated stop error means the old instance is not running, so a restart
// whose start leg then fails is handled as after a clean stop: the slot is
// released and the agent is recorded as stopped.
func TestAgentLifecycle_RestartToleratedStopThenFailedStartReleasesSlot(t *testing.T) {
	srv, s := testServer(t)
	disp := &tolerableStopFailingStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, project := newQuotaTestBrokerAndProject(t, s, "restart-tolerated-stop")
	running := newQuotaTestAgent(t, s, broker, project, "restart-tolerated-stop", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, running.ID)
	// The agent has a run, as every dispatched agent does: a failed restart
	// that leaves no run records nothing (ptone/scion#2550 P5, see
	// TestRestartStartLegFailureWithEmptyCurrentRunRecordsNothing).
	_, err := s.SetAgentRunID(context.Background(), running.ID, "run-x", nil)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+running.ID+"/restart", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.EqualValues(t, 1, disp.stopCount.Load(), "the stop leg is dispatched once")
	assert.EqualValues(t, 1, disp.startCount.Load(), "the start leg follows a tolerated stop error")

	assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID),
		"the slot is released after a tolerated stop and a failed start")
	got, err := s.GetAgent(context.Background(), running.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase,
		"the agent is recorded as stopped after a tolerated stop and a failed start")
}
