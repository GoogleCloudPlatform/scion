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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	}{
		{name: "clean stop", stopErr: nil, wantStatus: http.StatusOK, wantStart: true},
		{name: "agent not found", stopErr: brokerErr(http.StatusNotFound, ErrCodeAgentNotFound),
			wantStatus: http.StatusOK, wantStart: true},
		{name: "agent not running", stopErr: brokerErr(http.StatusConflict, ErrCodeAgentNotRunning),
			wantStatus: http.StatusOK, wantStart: true},
		{name: "generic error", stopErr: errors.New("broker unreachable"),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "broker 502", stopErr: brokerErr(http.StatusBadGateway, ErrCodeRuntimeError),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "404 without agent_not_found", stopErr: brokerErr(http.StatusNotFound, ErrCodeNotFound),
			wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "runtime unavailable", stopErr: brokerErr(http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable),
			wantStatus: http.StatusServiceUnavailable, wantCode: brokerCodeRuntimeUnavailable},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &runIntentDispatcher{stopErr: tc.stopErr}
			srv.SetDispatcher(disp)
			_, _, agent := setupOnlineBrokerAgent(t, s, "restart-stop-"+string(rune('a'+i)))

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

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase,
				"an aborted restart leaves the pre-restart phase: the instance may still be running")
		})
	}
}
