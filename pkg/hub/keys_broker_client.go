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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// Compile-time interface assertions for every agentkeys.BrokerClient/
// Dispatcher implementation this task ships. Production only reaches these
// adapters through a runtime type assertion (d.client.(agentkeys.BrokerClient),
// c.httpClient.(agentkeys.BrokerClient)) rather than a static interface-typed
// field, because BrokerClient/Dispatcher are deliberately new, standalone
// interfaces and not new methods on the existing, widely-implemented
// RuntimeBrokerClient/AgentDispatcher (contract §4.4). Without these
// assertions, a signature drift on any one adapter's ExecuteKeys/
// DispatchAgentKeys would still compile and pass every test that calls the
// concrete method directly or uses a fake — it would only surface in
// production as every keys request silently failing "wiring defect" ->
// ErrNotDispatched -> keys_unavailable, with no build-time signal.
var (
	_ agentkeys.BrokerClient = (*HTTPRuntimeBrokerClient)(nil)
	_ agentkeys.BrokerClient = (*AuthenticatedBrokerClient)(nil)
	_ agentkeys.BrokerClient = (*ControlChannelBrokerClient)(nil)
	_ agentkeys.BrokerClient = (*HybridBrokerClient)(nil)
	_ agentkeys.Dispatcher   = (*HTTPAgentDispatcher)(nil)
)

// decodeBrokerKeysResponse turns a raw HTTP-shaped response (status code and
// body) from a runtime broker's dedicated keys route into an
// (agentkeys.BrokerResult, error) pair, per the frozen classification rule in
// .design/agent-keys-contract.md §4.3. It is shared by every BrokerClient
// transport this task implements (HTTP and control-channel; HybridBrokerClient
// delegates to whichever of the two it routes to) so the classification
// decision is made in exactly one place, not re-derived per transport.
//
// Success is HTTP 200 with a body whose Outcome is agentkeys.OutcomeDispatched
// AND whose OperationID echoes expectedOperationID — the only success shape.
// A 200 that decodes but echoes back a different (or empty) operation ID is a
// broker contract violation, and the Hub cannot safely correlate it with its
// own audit record for this call, so it is treated the same as any other
// malformed 200: a plain, unclassified error (never a false success, and
// never a false "definitely didn't happen" either, since the broker may still
// have run some request's keys). Every other combination also returns a
// non-nil error:
//
//   - HTTP 404 whose body does not decode as a BrokerResult with a
//     agentkeys.ValidBrokerOutcome value is the one specified exception (an
//     old broker without the keys route at all, whose generic
//     unrecognized-action handler answers 404 with no top-level "outcome"
//     field): classified as *agentkeys.BrokerOutcomeError{Outcome:
//     agentkeys.OutcomeKeysUnsupported}.
//   - A response whose body decodes as a BrokerResult with a
//     agentkeys.ValidBrokerOutcome value, at the HTTP status
//     agentkeys.HTTPStatus(outcome) predicts, is a well-formed broker
//     decision: *agentkeys.BrokerOutcomeError{Outcome: outcome, Message:
//     message}.
//   - Anything else — an unparseable body on a non-404 status, a status/body
//     outcome mismatch, or a 2xx whose Outcome is not OutcomeDispatched — is a
//     plain, unclassified error. agentkeys.ClassifyDispatchError maps any
//     unrecognized error to OutcomeKeysOutcomeUnknown, which is exactly right
//     here: none of these shapes rule out that a real handler began executing
//     before producing a malformed response.
func decodeBrokerKeysResponse(statusCode int, body []byte, expectedOperationID string) (agentkeys.BrokerResult, error) {
	var result agentkeys.BrokerResult
	decodeErr := json.Unmarshal(body, &result)
	validBody := decodeErr == nil && result.Outcome != ""

	if statusCode == http.StatusOK {
		if validBody && result.Outcome == agentkeys.OutcomeDispatched {
			if result.OperationID != expectedOperationID {
				return agentkeys.BrokerResult{}, fmt.Errorf("keys: broker echoed operation_id %q, want %q", result.OperationID, expectedOperationID)
			}
			return result, nil
		}
		return agentkeys.BrokerResult{}, fmt.Errorf("keys: broker returned HTTP 200 with an unexpected body")
	}

	if statusCode == http.StatusNotFound && !(validBody && agentkeys.ValidBrokerOutcome(result.Outcome)) {
		return agentkeys.BrokerResult{}, &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeKeysUnsupported}
	}

	if validBody && agentkeys.ValidBrokerOutcome(result.Outcome) {
		if wantStatus, ok := agentkeys.HTTPStatus(result.Outcome); ok && wantStatus == statusCode {
			return agentkeys.BrokerResult{}, &agentkeys.BrokerOutcomeError{Outcome: result.Outcome, Message: result.Message}
		}
	}

	return agentkeys.BrokerResult{}, fmt.Errorf("keys: broker returned unexpected response (status %d)", statusCode)
}
