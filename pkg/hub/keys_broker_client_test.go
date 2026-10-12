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

package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// TestDecodeBrokerKeysResponse pins the wire-shape classification frozen by
// .design/agent-keys-contract.md §4.3: exactly one success shape, one
// specified 404 exception (old broker without the keys route), the broker's
// own well-formed allow-listed decisions, and a fail-safe "unknown" default
// for anything else. Every case is round-tripped through
// agentkeys.ClassifyDispatchError too, since that is what task 2.2 actually
// calls.
func TestDecodeBrokerKeysResponse(t *testing.T) {
	marshal := func(v interface{}) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal fixture: %v", err)
		}
		return b
	}

	cases := []struct {
		name                 string
		statusCode           int
		body                 []byte
		expectOperationID    string // defaults to "op-N" matching the fixture body below if empty
		wantSuccess          bool
		wantOutcome          agentkeys.Outcome // meaningful only when !wantSuccess and the error is a *BrokerOutcomeError
		wantBrokerOutcomeErr bool
	}{
		{
			name:        "200 dispatched is the only success shape",
			statusCode:  http.StatusOK,
			body:        marshal(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched}),
			wantSuccess: true,
		},
		{
			name:              "200 dispatched with a mismatched operation_id echo is unknown, not success",
			statusCode:        http.StatusOK,
			body:              marshal(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched}),
			expectOperationID: "op-999",
		},
		{
			name:              "200 dispatched with an empty operation_id echo is unknown, not success",
			statusCode:        http.StatusOK,
			body:              marshal(agentkeys.BrokerResult{OperationID: "", Outcome: agentkeys.OutcomeDispatched}),
			expectOperationID: "op-1",
		},
		{
			name:                 "old broker 404 with no outcome field is keys_unsupported",
			statusCode:           http.StatusNotFound,
			body:                 []byte(`{"error":{"code":"not_found","message":"Action not found"}}`),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeKeysUnsupported,
		},
		{
			name:                 "dedicated handler's own not_found at 404 is a broker decision",
			statusCode:           http.StatusNotFound,
			body:                 marshal(agentkeys.BrokerResult{OperationID: "op-2", Outcome: agentkeys.OutcomeNotFound}),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeNotFound,
		},
		{
			name:                 "agent_not_running at 409 is a broker decision",
			statusCode:           http.StatusConflict,
			body:                 marshal(agentkeys.BrokerResult{OperationID: "op-3", Outcome: agentkeys.OutcomeAgentNotRunning}),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeAgentNotRunning,
		},
		{
			name:                 "terminal_not_ready at 409 is a broker decision",
			statusCode:           http.StatusConflict,
			body:                 marshal(agentkeys.BrokerResult{OperationID: "op-4", Outcome: agentkeys.OutcomeTerminalNotReady}),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeTerminalNotReady,
		},
		{
			name:                 "keys_unsupported at 422 is a broker decision",
			statusCode:           http.StatusUnprocessableEntity,
			body:                 marshal(agentkeys.BrokerResult{OperationID: "op-5", Outcome: agentkeys.OutcomeKeysUnsupported}),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeKeysUnsupported,
		},
		{
			name:                 "keys_unavailable at 503 (expired deadline found at admission) is a broker decision",
			statusCode:           http.StatusServiceUnavailable,
			body:                 marshal(agentkeys.BrokerResult{OperationID: "op-6", Outcome: agentkeys.OutcomeKeysUnavailable}),
			wantBrokerOutcomeErr: true,
			wantOutcome:          agentkeys.OutcomeKeysUnavailable,
		},
		{
			name:       "status/body outcome mismatch is unknown, not a broker decision",
			statusCode: http.StatusNotFound,
			// A well-formed, allow-listed outcome whose expected status (409)
			// disagrees with the actual status (404): this must not be
			// silently reinterpreted as "not found".
			body: marshal(agentkeys.BrokerResult{OperationID: "op-7", Outcome: agentkeys.OutcomeAgentNotRunning}),
		},
		{
			name:       "2xx whose outcome is not dispatched is unknown, never a false success",
			statusCode: http.StatusOK,
			body:       marshal(agentkeys.BrokerResult{OperationID: "op-8", Outcome: agentkeys.OutcomeAgentNotRunning}),
		},
		{
			name:       "200 with unparseable body is unknown",
			statusCode: http.StatusOK,
			body:       []byte("not json"),
		},
		{
			name:       "generic 5xx with an unrelated body is unknown",
			statusCode: http.StatusBadGateway,
			body:       []byte(`{"error":{"code":"internal","message":"boom"}}`),
		},
		{
			name:       "outcome outside the broker-assertable allowlist is unknown even if status matches",
			statusCode: http.StatusBadRequest,
			body:       marshal(agentkeys.BrokerResult{OperationID: "op-9", Outcome: agentkeys.OutcomeInvalidRequest}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectOperationID := tc.expectOperationID
			if expectOperationID == "" {
				// Default to whatever operation_id the fixture body already
				// carries, so cases that aren't specifically testing the
				// echo check don't have to repeat it. Cases testing a
				// mismatch set expectOperationID explicitly above.
				var probe agentkeys.BrokerResult
				_ = json.Unmarshal(tc.body, &probe)
				expectOperationID = probe.OperationID
			}
			result, err := decodeBrokerKeysResponse(tc.statusCode, tc.body, expectOperationID)

			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				if result.Outcome != agentkeys.OutcomeDispatched {
					t.Fatalf("expected OutcomeDispatched, got %q", result.Outcome)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected an error, got success with result %+v", result)
			}

			var boe *agentkeys.BrokerOutcomeError
			isBrokerOutcomeErr := errors.As(err, &boe)
			if isBrokerOutcomeErr != tc.wantBrokerOutcomeErr {
				t.Fatalf("errors.As(*BrokerOutcomeError) = %v, want %v (err=%v)", isBrokerOutcomeErr, tc.wantBrokerOutcomeErr, err)
			}
			if tc.wantBrokerOutcomeErr && boe.Outcome != tc.wantOutcome {
				t.Fatalf("BrokerOutcomeError.Outcome = %q, want %q", boe.Outcome, tc.wantOutcome)
			}

			gotClassified := agentkeys.ClassifyDispatchError(err)
			wantClassified := agentkeys.OutcomeKeysOutcomeUnknown
			if tc.wantBrokerOutcomeErr {
				wantClassified = tc.wantOutcome
			}
			if gotClassified != wantClassified {
				t.Fatalf("ClassifyDispatchError(err) = %q, want %q", gotClassified, wantClassified)
			}
		})
	}
}
